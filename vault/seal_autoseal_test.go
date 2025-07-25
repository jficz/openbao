// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/armon/go-metrics"
	"github.com/openbao/openbao/helper/metricsutil"
	"google.golang.org/protobuf/proto"

	wrapping "github.com/openbao/go-kms-wrapping/v2"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/vault/seal"
)

// phy implements physical.Backend. It maps keys to a slice of entries.
// Each call to Put appends the entry to the slice of entries for that
// key. No deduplication is done. This allows the test for UpgradeKeys to
// verify entries are only being updated when the underlying encryption key
// has been updated.
type phy struct {
	t       *testing.T
	entries map[string][]*physical.Entry
}

var _ physical.Backend = (*phy)(nil)

func newTestBackend(t *testing.T) *phy {
	return &phy{
		t:       t,
		entries: make(map[string][]*physical.Entry),
	}
}

func (p *phy) Put(_ context.Context, entry *physical.Entry) error {
	p.entries[entry.Key] = append(p.entries[entry.Key], entry)
	return nil
}

func (p *phy) Get(_ context.Context, key string) (*physical.Entry, error) {
	entries := p.entries[key]
	if entries == nil {
		return nil, nil
	}
	return entries[len(entries)-1], nil
}

func (p *phy) Delete(_ context.Context, key string) error {
	p.t.Errorf("Delete called on phy: key: %v", key)
	return nil
}

func (p *phy) List(_ context.Context, prefix string) ([]string, error) {
	p.t.Errorf("List called on phy: prefix: %v", prefix)
	return []string{}, nil
}

func (p *phy) ListPage(_ context.Context, prefix string, after string, limit int) ([]string, error) {
	p.t.Errorf("ListPage called on phy: prefix: %v", prefix)
	return []string{}, nil
}

func (p *phy) Len() int {
	return len(p.entries)
}

func TestAutoSeal_UpgradeKeys(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	testSeal, toggleableWrapper := seal.NewTestSeal(nil)

	var encKeys []string
	changeKey := func(key string) {
		encKeys = append(encKeys, key)
		toggleableWrapper.Wrapper.(*wrapping.TestWrapper).SetKeyId(key)
	}

	// Set initial encryption key.
	changeKey("kaz")

	autoSeal, err := NewAutoSeal(testSeal)
	if err != nil {
		t.Fatal(err)
	}

	autoSeal.SetCore(core)
	pBackend := newTestBackend(t)
	core.physical = pBackend

	ctx := context.Background()

	inkeys := [][]byte{[]byte("grist"), []byte("house")}
	if err := autoSeal.SetStoredKeys(ctx, inkeys); err != nil {
		t.Fatalf("SetStoredKeys: want no error, got %v", err)
	}

	inRecoveryKey := []byte("falernum")
	if err := autoSeal.SetRecoveryKey(ctx, inRecoveryKey); err != nil {
		t.Fatalf("SetRecoveryKey: want no error, got %v", err)
	}

	check := func() {
		// The values of the stored keys should never change.
		outkeys, err := autoSeal.GetStoredKeys(ctx)
		if err != nil {
			t.Fatalf("GetStoredKeys: want no error, got %v", err)
		}
		if !reflect.DeepEqual(inkeys, outkeys) {
			t.Errorf("incorrect stored keys: want %v, got %v", inkeys, outkeys)
		}

		// The value of the recovery key should also never change.
		outRecoveryKey, err := autoSeal.RecoveryKey(ctx)
		if err != nil {
			t.Fatalf("RecoveryKey: want no error, got %v", err)
		}
		if !bytes.Equal(inRecoveryKey, outRecoveryKey) {
			t.Errorf("incorrect recovery key: want %q, got %q", inRecoveryKey, outRecoveryKey)
		}

		// There should only be 2 entries in the physical backend. One for
		// the stored keys and one for the recovery key.
		if want, got := 2, pBackend.Len(); want != got {
			t.Errorf("backend unexpected Len: want %d, got %d", want, got)
		}

		for phyKey, phyEntries := range pBackend.entries {
			// Calling UpgradeKeys should only add an entry if the key has
			// changed.
			if keyCount, entryCount := len(encKeys), len(phyEntries); keyCount != entryCount {
				t.Errorf("phyKey = %s: encryption key count not equal to entry count: keys=%d, entries=%d", phyKey, keyCount, entryCount)
			}

			// Each phyEntry should correspond to a key at the same index
			// in encKeys. Iterate over each phyEntry and verify it was
			// encrypted with its corresponding key in encKeys.
			for i, phyEntry := range phyEntries {
				blobInfo := &wrapping.BlobInfo{}
				if err := proto.Unmarshal(phyEntry.Value, blobInfo); err != nil {
					t.Errorf("phyKey = %s: failed to proto decode stored keys: %s", phyKey, err)
				}
				if blobInfo.KeyInfo == nil {
					t.Errorf("phyKey = %s: KeyInfo missing: %+v", phyKey, blobInfo)
				}
				if want, got := encKeys[i], blobInfo.KeyInfo.KeyId; want != got {
					t.Errorf("phyKey = %s: Incorrect encryption key: want %s, got %s", phyKey, want, got)
				}
			}
		}
	}

	// Verify the current state is correct before calling UpgradeKeys.
	check()

	// Call UpgradeKeys before changing the encryption key and verify
	// nothing has changed.
	if err := autoSeal.UpgradeKeys(ctx); err != nil {
		t.Fatalf("UpgradeKeys: want no error, got %v", err)
	}
	check()

	// Change the encryption key, call UpgradeKeys, then verify the stored
	// keys and recovery key has been re-encrypted with the new encryption
	// key.
	changeKey("primanti")
	if err := autoSeal.UpgradeKeys(ctx); err != nil {
		t.Fatalf("UpgradeKeys: want no error, got %v", err)
	}
	check()
}

func TestAutoSeal_HealthCheck(t *testing.T) {
	inmemSink := metrics.NewInmemSink(
		1000000*time.Hour,
		2000000*time.Hour)

	metricsConf := metrics.DefaultConfig("")
	metricsConf.EnableHostname = false
	metricsConf.EnableHostnameLabel = false
	metricsConf.EnableServiceLabel = false
	metricsConf.EnableTypePrefix = false

	metrics.NewGlobal(metricsConf, inmemSink)

	pBackend := newTestBackend(t)
	testSealAccess, setErr := seal.NewToggleableTestSeal(nil)
	core, _, _ := TestCoreUnsealedWithConfig(t, &CoreConfig{
		MetricSink: metricsutil.NewClusterMetricSink("", inmemSink),
		Physical:   pBackend,
	})
	sealHealthTestIntervalNominal = 10 * time.Millisecond
	sealHealthTestIntervalUnhealthy = 10 * time.Millisecond
	autoSeal, err := NewAutoSeal(testSealAccess)
	if err != nil {
		t.Fatal(err)
	}

	autoSeal.SetCore(core)
	core.seal = autoSeal
	autoSeal.StartHealthCheck()
	defer autoSeal.StopHealthCheck()
	setErr(errors.New("disconnected"))

	asu := strings.Join(autoSealUnavailableDuration, ".") + ";cluster=" + core.clusterName
	tries := 10
	for tries = 10; tries > 0; tries-- {
		intervals := inmemSink.Data()
		if len(intervals) == 1 {
			interval := inmemSink.Data()[0]

			if _, ok := interval.Gauges[asu]; ok {
				if interval.Gauges[asu].Value > 0 {
					break
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if tries == 0 {
		t.Fatalf("Expected value metric %s to be non-zero", asu)
	}

	setErr(nil)
	time.Sleep(50 * time.Millisecond)
	intervals := inmemSink.Data()
	if len(intervals) == 1 {
		interval := inmemSink.Data()[0]

		if _, ok := interval.Gauges[asu]; !ok {
			t.Fatalf("Expected metrics to include a value for gauge %s", asu)
		}
		if interval.Gauges[asu].Value != 0 {
			t.Fatalf("Expected value metric %s to be zero", asu)
		}
	}
}
