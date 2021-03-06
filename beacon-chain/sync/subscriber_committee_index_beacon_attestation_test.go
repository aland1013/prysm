package sync

import (
	"context"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-bitfield"
	mock "github.com/prysmaticlabs/prysm/beacon-chain/blockchain/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed"
	statefeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/state"
	dbtest "github.com/prysmaticlabs/prysm/beacon-chain/db/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations/attestations"
	p2ptest "github.com/prysmaticlabs/prysm/beacon-chain/p2p/testing"
	mockSync "github.com/prysmaticlabs/prysm/beacon-chain/sync/initial-sync/testing"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/testutil"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stateutil"
)

func TestService_committeeIndexBeaconAttestationSubscriber_ValidMessage(t *testing.T) {
	p := p2ptest.NewTestP2P(t)
	resetCfg := featureconfig.InitWithReset(&featureconfig.Flags{DisableDynamicCommitteeSubnets: true})
	defer resetCfg()

	ctx := context.Background()
	db := dbtest.SetupDB(t)
	s, sKeys := testutil.DeterministicGenesisState(t, 64 /*validators*/)
	if err := s.SetGenesisTime(uint64(time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	blk, err := testutil.GenerateFullBlock(s, sKeys, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	root, err := stateutil.BlockRoot(blk.Block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBlock(ctx, blk); err != nil {
		t.Fatal(err)
	}

	savedState := testutil.NewBeaconState()
	if err := db.SaveState(context.Background(), savedState, root); err != nil {
		t.Fatal(err)
	}

	c, err := lru.New(10)
	if err != nil {
		t.Fatal(err)
	}
	r := &Service{
		attPool: attestations.NewPool(),
		chain: &mock.ChainService{
			State:            s,
			Genesis:          time.Now(),
			ValidAttestation: true,
			ValidatorsRoot:   [32]byte{'A'},
		},
		chainStarted:         true,
		p2p:                  p,
		db:                   db,
		ctx:                  ctx,
		stateNotifier:        (&mock.ChainService{}).StateNotifier(),
		attestationNotifier:  (&mock.ChainService{}).OperationNotifier(),
		initialSync:          &mockSync.Sync{IsSyncing: false},
		seenAttestationCache: c,
		stateSummaryCache:    cache.NewStateSummaryCache(),
	}
	p.Digest, err = r.forkDigest()
	if err != nil {
		t.Fatal(err)
	}
	r.registerSubscribers()
	r.stateNotifier.StateFeed().Send(&feed.Event{
		Type: statefeed.Initialized,
		Data: &statefeed.InitializedData{
			StartTime: time.Now(),
		},
	})

	att := &eth.Attestation{
		Data: &eth.AttestationData{
			Slot:            0,
			BeaconBlockRoot: root[:],
		},
		AggregationBits: bitfield.Bitlist{0b0101},
		Signature:       sKeys[0].Sign([]byte("foo")).Marshal(),
	}

	p.ReceivePubSub("/eth2/%x/committee_index0_beacon_attestation", att)

	time.Sleep(time.Second)

	ua := r.attPool.UnaggregatedAttestations()
	if len(ua) == 0 {
		t.Error("No attestations put into pool")
	}
}
