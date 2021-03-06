package detection

import (
	"context"

	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/sliceutil"
	"github.com/prysmaticlabs/prysm/slasher/beaconclient"
	"github.com/prysmaticlabs/prysm/slasher/db"
	"github.com/prysmaticlabs/prysm/slasher/detection/attestations"
	"github.com/prysmaticlabs/prysm/slasher/detection/attestations/iface"
	"github.com/prysmaticlabs/prysm/slasher/detection/proposals"
	proposerIface "github.com/prysmaticlabs/prysm/slasher/detection/proposals/iface"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

var log = logrus.WithField("prefix", "detection")

// Service struct for the detection service of the slasher.
type Service struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	slasherDB             db.Database
	blocksChan            chan *ethpb.SignedBeaconBlock
	attsChan              chan *ethpb.IndexedAttestation
	notifier              beaconclient.Notifier
	chainFetcher          beaconclient.ChainFetcher
	beaconClient          *beaconclient.Service
	attesterSlashingsFeed *event.Feed
	proposerSlashingsFeed *event.Feed
	minMaxSpanDetector    iface.SpanDetector
	proposalsDetector     proposerIface.ProposalsDetector
}

// Config options for the detection service.
type Config struct {
	Notifier              beaconclient.Notifier
	SlasherDB             db.Database
	ChainFetcher          beaconclient.ChainFetcher
	BeaconClient          *beaconclient.Service
	AttesterSlashingsFeed *event.Feed
	ProposerSlashingsFeed *event.Feed
}

// NewDetectionService instantiation.
func NewDetectionService(ctx context.Context, cfg *Config) *Service {
	ctx, cancel := context.WithCancel(ctx)
	return &Service{
		ctx:                   ctx,
		cancel:                cancel,
		notifier:              cfg.Notifier,
		chainFetcher:          cfg.ChainFetcher,
		slasherDB:             cfg.SlasherDB,
		beaconClient:          cfg.BeaconClient,
		blocksChan:            make(chan *ethpb.SignedBeaconBlock, 1),
		attsChan:              make(chan *ethpb.IndexedAttestation, 1),
		attesterSlashingsFeed: cfg.AttesterSlashingsFeed,
		proposerSlashingsFeed: cfg.ProposerSlashingsFeed,
		minMaxSpanDetector:    attestations.NewSpanDetector(cfg.SlasherDB),
		proposalsDetector:     proposals.NewProposeDetector(cfg.SlasherDB),
	}
}

// Stop the notifier service.
func (ds *Service) Stop() error {
	ds.cancel()
	log.Info("Stopping service")
	return nil
}

// Status returns an error if there exists an error in
// the notifier service.
func (ds *Service) Status() error {
	return nil
}

// Start the detection service runtime.
func (ds *Service) Start() {
	// We wait for the gRPC beacon client to be ready and the beacon node
	// to be fully synced before proceeding.
	ch := make(chan bool)
	sub := ds.notifier.ClientReadyFeed().Subscribe(ch)
	<-ch
	sub.Unsubscribe()

	if !featureconfig.Get().DisableHistoricalDetection {
		// The detection service runs detection on all historical
		// chain data since genesis.
		go ds.detectHistoricalChainData(ds.ctx)
	}

	// We subscribe to incoming blocks from the beacon node via
	// our gRPC client to keep detecting slashable offenses.
	go ds.detectIncomingBlocks(ds.ctx, ds.blocksChan)
	go ds.detectIncomingAttestations(ds.ctx, ds.attsChan)
}

func (ds *Service) detectHistoricalChainData(ctx context.Context) {
	ctx, span := trace.StartSpan(ctx, "detection.detectHistoricalChainData")
	defer span.End()
	// We fetch both the latest persisted chain head in our DB as well
	// as the current chain head from the beacon node via gRPC.
	latestStoredHead, err := ds.slasherDB.ChainHead(ctx)
	if err != nil {
		log.WithError(err).Fatal("Could not retrieve chain head from DB")
	}
	currentChainHead, err := ds.chainFetcher.ChainHead(ctx)
	if err != nil {
		log.WithError(err).Fatal("Cannot retrieve chain head from beacon node")
	}
	var latestStoredEpoch uint64
	if latestStoredHead != nil {
		latestStoredEpoch = latestStoredHead.HeadEpoch
	}

	// We retrieve historical chain data from the last persisted chain head in the
	// slasher DB up to the current beacon node's head epoch we retrieved via gRPC.
	// If no data was persisted from previous sessions, we request data starting from
	// the genesis epoch.
	for epoch := latestStoredEpoch; epoch < currentChainHead.HeadEpoch; epoch++ {
		indexedAtts, err := ds.beaconClient.RequestHistoricalAttestations(ctx, epoch)
		if err != nil {
			log.WithError(err).Errorf("Could not fetch attestations for epoch: %d", epoch)
		}
		log.Debugf(
			"Running slashing detection on %d attestations in epoch %d...",
			len(indexedAtts),
			epoch,
		)

		for _, att := range indexedAtts {
			slashings, err := ds.DetectAttesterSlashings(ctx, att)
			if err != nil {
				log.WithError(err).Error("Could not detect attester slashings")
				continue
			}
			ds.submitAttesterSlashings(ctx, slashings)
		}
		latestStoredHead = &ethpb.ChainHead{HeadEpoch: epoch}
		if err := ds.slasherDB.SaveChainHead(ctx, latestStoredHead); err != nil {
			log.WithError(err).Error("Could not persist chain head to disk")
		}
	}
	log.Infof("Completed slashing detection on historical chain data up to epoch %d", currentChainHead.HeadEpoch)
}

func (ds *Service) submitAttesterSlashings(ctx context.Context, slashings []*ethpb.AttesterSlashing) {
	ctx, span := trace.StartSpan(ctx, "detection.submitAttesterSlashings")
	defer span.End()
	for i := 0; i < len(slashings); i++ {
		slash := slashings[i]
		if slash != nil && slash.Attestation_1 != nil && slash.Attestation_2 != nil {
			slashableIndices := sliceutil.IntersectionUint64(slashings[i].Attestation_1.AttestingIndices, slashings[i].Attestation_2.AttestingIndices)
			log.WithFields(logrus.Fields{
				"sourceEpoch":  slash.Attestation_1.Data.Source.Epoch,
				"targetEpoch":  slash.Attestation_1.Data.Target.Epoch,
				"surroundVote": isSurrounding(slash.Attestation_1, slash.Attestation_2),
				"indices":      slashableIndices,
			}).Info("Found an attester slashing! Submitting to beacon node")
			ds.attesterSlashingsFeed.Send(slashings[i])
		}
	}
}

func (ds *Service) submitProposerSlashing(ctx context.Context, slashing *ethpb.ProposerSlashing) {
	ctx, span := trace.StartSpan(ctx, "detection.submitProposerSlashing")
	defer span.End()
	if slashing != nil && slashing.Header_1 != nil && slashing.Header_2 != nil {
		log.WithFields(logrus.Fields{
			"header1Slot":        slashing.Header_1.Header.Slot,
			"header2Slot":        slashing.Header_2.Header.Slot,
			"proposerIdxHeader1": slashing.Header_1.Header.ProposerIndex,
			"proposerIdxHeader2": slashing.Header_2.Header.ProposerIndex,
		}).Info("Found a proposer slashing! Submitting to beacon node")
		ds.proposerSlashingsFeed.Send(slashing)
	}
}
