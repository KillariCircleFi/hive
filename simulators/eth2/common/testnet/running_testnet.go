package testnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/pkg/errors"

	"github.com/protolambda/eth2api"
	"github.com/protolambda/zrnt/eth2/beacon/altair"
	"github.com/protolambda/zrnt/eth2/beacon/common"
	"github.com/protolambda/zrnt/eth2/beacon/phase0"
	"github.com/protolambda/zrnt/eth2/util/math"
	"github.com/protolambda/ztyp/tree"

	"github.com/ethereum/hive/hivesim"
	execution_config "github.com/ethereum/hive/simulators/eth2/common/config/execution"
	"github.com/ethereum/hive/simulators/eth2/common/utils"
	"github.com/marioevz/blobber"
	blobber_config "github.com/marioevz/blobber/config"
	"github.com/marioevz/blobber/keys"
	beacon_client "github.com/marioevz/eth-clients/clients/beacon"
	exec_client "github.com/marioevz/eth-clients/clients/execution"
	node "github.com/marioevz/eth-clients/clients/node"
	builder_types "github.com/marioevz/mock-builder/types"
)

const (
	MAX_PARTICIPATION_SCORE = 7
)

var (
	EMPTY_EXEC_HASH = ethcommon.Hash{}
	EMPTY_TREE_ROOT = tree.Root{}
	JWT_SECRET, _   = hex.DecodeString(
		"7365637265747365637265747365637265747365637265747365637265747365",
	)
)

type Testnet struct {
	*hivesim.T
	node.Nodes

	genesisTime           common.Timestamp
	genesisValidatorsRoot common.Root

	// Consensus chain configuration
	spec *common.Spec
	// Execution chain configuration and genesis info
	executionGenesis *execution_config.ExecutionGenesis
	// Consensus genesis state
	eth2GenesisState common.BeaconState

	// Blobber
	blobber *blobber.Blobber

	// Test configuration
	maxConsecutiveErrorsOnWaits int

	// Validators
	Validators      *utils.Validators
	ValidatorGroups map[string]*utils.Validators
}

type ActiveSpec struct {
	*common.Spec
}

const slotsTolerance common.Slot = 2

func (spec *ActiveSpec) EpochTimeoutContext(
	parent context.Context,
	epochs common.Epoch,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		parent,
		time.Duration(
			uint64((spec.SLOTS_PER_EPOCH*common.Slot(epochs))+slotsTolerance)*
				uint64(spec.SECONDS_PER_SLOT),
		)*time.Second,
	)
}

func (spec *ActiveSpec) SlotTimeoutContext(
	parent context.Context,
	slots common.Slot,
) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		parent,
		time.Duration(
			uint64(slots+slotsTolerance)*
				uint64(spec.SECONDS_PER_SLOT))*time.Second,
	)
}

func (spec *ActiveSpec) EpochsTimeout(epochs common.Epoch) <-chan time.Time {
	return time.After(
		time.Duration(
			uint64(
				spec.SLOTS_PER_EPOCH*common.Slot(epochs),
			)*uint64(
				spec.SECONDS_PER_SLOT,
			),
		) * time.Second,
	)
}

func (spec *ActiveSpec) SlotsTimeout(slots common.Slot) <-chan time.Time {
	return time.After(
		time.Duration(
			uint64(slots)*uint64(spec.SECONDS_PER_SLOT),
		) * time.Second,
	)
}

func (t *Testnet) Spec() *ActiveSpec {
	return &ActiveSpec{
		Spec: t.spec,
	}
}

func (t *Testnet) GenesisTime() common.Timestamp {
	// return time.Unix(int64(t.genesisTime), 0)
	return t.genesisTime
}

func (t *Testnet) GenesisTimeUnix() time.Time {
	return time.Unix(int64(t.genesisTime), 0)
}

func (t *Testnet) GenesisBeaconState() common.BeaconState {
	return t.eth2GenesisState
}

func (t *Testnet) GenesisValidatorsRoot() common.Root {
	return t.genesisValidatorsRoot
}

func (t *Testnet) ExecutionGenesis() *core.Genesis {
	return t.executionGenesis.Genesis
}

func (t *Testnet) Blobber() *blobber.Blobber {
	return t.blobber
}

func StartTestnet(
	parentCtx context.Context,
	t *hivesim.T,
	env *Environment,
	config *Config,
) *Testnet {
	prep, err := PrepareTestnet(env, config)
	if err != nil {
		t.Fatalf("FAIL: Unable to prepare testnet: %v", err)
	}
	var (
		testnet     = prep.createTestnet(t)
		genesisTime = testnet.GenesisTimeUnix()
	)
	t.Logf(
		"Created new testnet, genesis at %s (%s from now)",
		genesisTime,
		time.Until(genesisTime),
	)

	var simulatorIP net.IP
	if simIPStr, err := t.Sim.ContainerNetworkIP(
		testnet.T.SuiteID,
		"bridge",
		"simulation",
	); err != nil {
		panic(err)
	} else {
		simulatorIP = net.ParseIP(simIPStr)
	}

	testnet.Nodes = make(node.Nodes, len(config.NodeDefinitions))

	// Init all client bundles
	for nodeIndex := range testnet.Nodes {
		testnet.Nodes[nodeIndex] = new(node.Node)
	}

	if config.EnableBlobber {
		blobberKeys := make([]*keys.ValidatorKey, 0)
		for _, key := range env.Validators {
			validator := new(keys.ValidatorKey)
			validator.FromBytes(key.ValidatorSecretKey[:])
			blobberKeys = append(blobberKeys, validator)
		}

		blobberOpts := []blobber_config.Option{
			blobber_config.WithExternalIP(simulatorIP),
			blobber_config.WithBeaconGenesisTime(testnet.genesisTime),
			blobber_config.WithSpec(prep.Spec),
			blobber_config.WithValidatorKeysList(blobberKeys),
			blobber_config.WithGenesisValidatorsRoot(testnet.genesisValidatorsRoot),
			blobber_config.WithLogLevel(getLogLevelString()),
		}
		blobberOpts = append(blobberOpts, config.BlobberOptions...)

		testnet.blobber, err = blobber.NewBlobber(parentCtx, blobberOpts...)
		if err != nil {
			t.Fatalf("FAIL: Unable to create blobber: %v", err)
		}

		// Add the blobber as trusted peer to the beacon nodes
		ids := testnet.blobber.GetNextPeerIDs(5) // Five should be enough for any test for now
		prep.beaconOpts = hivesim.Bundle(prep.beaconOpts,
			hivesim.Params{
				"HIVE_ETH2_TRUSTED_PEER_IDS": strings.Join(ids, ","),
			})
	}

	// For each key partition, we start a client bundle that consists of:
	// - 1 execution client
	// - 1 beacon client
	// - 1 validator client,
	for nodeIndex, node := range config.NodeDefinitions {
		// Prepare clients for this node
		var (
			nodeClient = testnet.Nodes[nodeIndex]

			executionDef = env.Clients.ClientByNameAndRole(
				node.ExecutionClientName(),
				"eth1",
			)
			beaconDef = env.Clients.ClientByNameAndRole(
				node.ConsensusClientName(),
				"beacon",
			)
			validatorDef = env.Clients.ClientByNameAndRole(
				node.ValidatorClientName(),
				"validator",
			)
			executionTTD = int64(0)
			beaconTTD    = int64(0)
		)

		if executionDef == nil || beaconDef == nil || validatorDef == nil {
			t.Fatalf("FAIL: Unable to get client")
		}
		if node.ExecutionClientTTD != nil {
			executionTTD = node.ExecutionClientTTD.Int64()
		} else if testnet.executionGenesis.Genesis.Config.TerminalTotalDifficulty != nil {
			executionTTD = testnet.executionGenesis.Genesis.Config.TerminalTotalDifficulty.Int64()
		}
		if node.BeaconNodeTTD != nil {
			beaconTTD = node.BeaconNodeTTD.Int64()
		} else if testnet.executionGenesis.Genesis.Config.TerminalTotalDifficulty != nil {
			beaconTTD = testnet.executionGenesis.Genesis.Config.TerminalTotalDifficulty.Int64()
		}

		// Prepare the client objects with all the information necessary to
		// eventually start
		nodeClient.ExecutionClient = prep.prepareExecutionNode(
			parentCtx,
			testnet,
			executionDef,
			config.Eth1Consensus,
			node.Chain,
			exec_client.ExecutionClientConfig{
				ClientIndex:             nodeIndex,
				TerminalTotalDifficulty: executionTTD,
				Subnet:                  node.GetExecutionSubnet(),
				JWTSecret:               JWT_SECRET,
				ProxyConfig: &exec_client.ExecutionProxyConfig{
					Host:                   simulatorIP,
					Port:                   exec_client.PortEngineRPC + nodeIndex,
					TrackForkchoiceUpdated: false,
					LogEngineCalls:         env.LogEngineCalls,
				},
			},
		)

		if node.ConsensusClient != "" {
			nodeClient.BeaconClient = prep.prepareBeaconNode(
				parentCtx,
				testnet,
				beaconDef,
				config.EnableBuilders,
				config.BuilderOptions,
				beacon_client.BeaconClientConfig{
					ClientIndex:             nodeIndex,
					BeaconAPIPort:           beacon_client.PortBeaconAPI,
					TerminalTotalDifficulty: beaconTTD,
					Spec:                    testnet.spec,
					GenesisValidatorsRoot:   &testnet.genesisValidatorsRoot,
					GenesisTime:             &testnet.genesisTime,
					Subnet:                  node.GetConsensusSubnet(),
				},
				nodeClient.ExecutionClient,
			)

			nodeClient.ValidatorClient = prep.prepareValidatorClient(
				parentCtx,
				testnet,
				validatorDef,
				nodeClient.BeaconClient,
				nodeIndex,
			)
		}

		// Add rest of properties
		nodeClient.Logging = t
		nodeClient.Index = nodeIndex
		nodeClient.Verification = node.TestVerificationNode
		// Start the node clients if specified so
		if !node.DisableStartup {
			t.Logf("Starting node %d", nodeIndex)
			if err := nodeClient.Start(); err != nil {
				t.Fatalf("FAIL: Unable to start node %d: %v", nodeIndex, err)
			}
		} else {
			t.Logf("Node %d startup disabled, skipping", nodeIndex)
		}
	}

	return testnet
}

func (t *Testnet) Stop() {
	for _, p := range t.Proxies().Running() {
		p.Cancel()
	}
	for _, b := range t.BeaconClients() {
		if b.Builder != nil {
			if builder, ok := b.Builder.(builder_types.Builder); ok {
				builder.Cancel()
			}
		}
	}

	if t.blobber != nil {
		t.blobber.Close()
	}
}

func (t *Testnet) ValidatorClientIndex(pk [48]byte) (int, error) {
	for i, v := range t.ValidatorClients() {
		if v.ContainsKey(pk) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("key not found in any validator client")
}

// Wait until the beacon chain genesis happens.
func (t *Testnet) WaitForGenesis(ctx context.Context) {
	genesis := t.GenesisTimeUnix()
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(genesis)):
	}
}

// Wait a certain amount of slots while printing the current status.
func (t *Testnet) WaitSlots(ctx context.Context, slots common.Slot) error {
	for s := common.Slot(0); s < slots; s++ {
		t.BeaconClients().Running().PrintStatus(ctx)
		select {
		case <-time.After(time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (t *Testnet) WaitSlotsWithMaxMissedSlots(ctx context.Context, slots common.Slot, maxMissedSlots common.Slot) error {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		slotsPassed  = common.Slot(0)
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(runningNodes, t.maxConsecutiveErrorsOnWaits)
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(
					ctx context.Context,
					n *node.Node,
					r *result,
				) {
					defer wg.Done()

					b := n.BeaconClient

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					versionedBlock, err := b.BlockV2(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(err, "failed to retrieve block")
						return
					}

					execution := ethcommon.Hash{}
					if executionPayload, _, _, err := versionedBlock.ExecutionPayload(); err == nil {
						execution = executionPayload.BlockHash
					}

					slot := versionedBlock.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= maxMissedSlots {
						r.fatal = fmt.Errorf(
							"missed more slots than allowed (max=%d): clockSlot=%d, slot=%d",
							maxMissedSlots,
							clockSlot,
							slot,
						)
						return
					}

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%s, slot=%d, head=%s, exec_payload=%s, justified=%s, finalized=%s",
						versionedBlock.Version,
						clockSlot,
						slot,
						utils.Shorten(versionedBlock.Root().String()),
						utils.Shorten(execution.String()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
					)
				}(ctx, n, results[i])
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return err
			}
			results.PrintMessages(t.Logf)
			slotsPassed += 1
			if slotsPassed >= slots {
				return nil
			}
		}
	}
}

// WaitForFork blocks until a beacon client reaches specified fork,
// or context finalizes, whichever happens first.
func (t *Testnet) WaitForFork(ctx context.Context, fork string) error {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(runningNodes, t.maxConsecutiveErrorsOnWaits)
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(
					ctx context.Context,
					n *node.Node,
					r *result,
				) {
					defer wg.Done()

					b := n.BeaconClient

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					versionedBlock, err := b.BlockV2(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(err, "failed to retrieve block")
						return
					}

					execution := ethcommon.Hash{}
					if executionPayload, _, _, err := versionedBlock.ExecutionPayload(); err == nil {
						execution = executionPayload.BlockHash
					}

					slot := versionedBlock.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= t.spec.SLOTS_PER_EPOCH {
						r.fatal = fmt.Errorf(
							"unable to sync for an entire epoch: clockSlot=%d, slot=%d",
							clockSlot,
							slot,
						)
						return
					}

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%s, slot=%d, head=%s, exec_payload=%s, justified=%s, finalized=%s",
						versionedBlock.Version,
						clockSlot,
						slot,
						utils.Shorten(versionedBlock.Root().String()),
						utils.Shorten(execution.String()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
					)

					if versionedBlock.Version == fork {
						r.done = true
					}
				}(ctx, n, results[i])
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return err
			}
			results.PrintMessages(t.Logf)
			if results.AllDone() {
				return nil
			}
		}
	}
}

// WaitForFinality blocks until a beacon client reaches finality,
// or timeoutSlots have passed, whichever happens first.
func (t *Testnet) WaitForFinality(ctx context.Context) (
	common.Checkpoint, error,
) {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(runningNodes, t.maxConsecutiveErrorsOnWaits)
	)

	for {
		select {
		case <-ctx.Done():
			return common.Checkpoint{}, ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(ctx context.Context, n *node.Node, r *result) {
					defer wg.Done()

					b := n.BeaconClient

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					versionedBlock, err := b.BlockV2(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(err, "failed to retrieve block")
						return
					}
					execution := ethcommon.Hash{}
					if executionPayload, _, _, err := versionedBlock.ExecutionPayload(); err == nil {
						execution = executionPayload.BlockHash
					}

					slot := versionedBlock.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= t.spec.SLOTS_PER_EPOCH {
						r.fatal = fmt.Errorf(
							"unable to sync for an entire epoch: clockSlot=%d, slot=%d",
							clockSlot,
							slot,
						)
						return
					}

					health, _ := GetHealth(ctx, b, t.spec, slot)

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%d, slot=%d, head=%s, "+
							"health=%.2f, exec_payload=%s, justified=%s, "+
							"finalized=%s",
						versionedBlock.Version,
						clockSlot,
						slot,
						utils.Shorten(versionedBlock.Root().String()),
						health,
						utils.Shorten(execution.String()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
					)

					if (checkpoints.Finalized != common.Checkpoint{}) {
						r.done = true
						r.result = checkpoints.Finalized
					}
				}(ctx, n, results[i])
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return common.Checkpoint{}, err
			}
			results.PrintMessages(t.Logf)
			if results.AllDone() {
				if cp, ok := results[0].result.(common.Checkpoint); ok {
					return cp, nil
				}
			}
		}
	}
}

// WaitForSync blocks until all beacon clients converge to the same head.
func (t *Testnet) WaitForSync(ctx context.Context) (
	tree.Root, error,
) {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(runningNodes, t.maxConsecutiveErrorsOnWaits)
	)

	for {
		select {
		case <-ctx.Done():
			return tree.Root{}, ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
				heads = make(chan tree.Root, len(runningNodes))
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(ctx context.Context, n *node.Node, r *result) {
					defer wg.Done()

					b := n.BeaconClient

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					versionedBlock, err := b.BlockV2(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(err, "failed to retrieve block")
						return
					}
					heads <- versionedBlock.Root()

					execution := ethcommon.Hash{}
					if executionPayload, _, _, err := versionedBlock.ExecutionPayload(); err == nil {
						execution = executionPayload.BlockHash
					}

					slot := versionedBlock.Slot()
					health, _ := GetHealth(ctx, b, t.spec, slot)

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%d, slot=%d, head=%s, "+
							"health=%.2f, exec_payload=%s, justified=%s, "+
							"finalized=%s",
						versionedBlock.Version,
						clockSlot,
						slot,
						utils.Shorten(versionedBlock.Root().String()),
						health,
						utils.Shorten(execution.String()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
					)

					if (checkpoints.Finalized != common.Checkpoint{}) {
						r.done = true
						r.result = checkpoints.Finalized
					}
				}(ctx, n, results[i])
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return tree.Root{}, err
			}
			results.PrintMessages(t.Logf)

			// Check if all heads are the same
			close(heads)
			var (
				head tree.Root
				ok   bool = true
			)
			for h := range heads {
				if head == EMPTY_TREE_ROOT {
					head = h
					continue
				}
				if !bytes.Equal(head[:], h[:]) {
					ok = false
					break
				}
			}
			if ok && head != EMPTY_TREE_ROOT {
				return head, nil
			}
		}
	}
}

// WaitForExecutionFinality blocks until a beacon client reaches finality
// and the finality checkpoint contains an execution payload,
// or timeoutSlots have passed, whichever happens first.
func (t *Testnet) WaitForExecutionFinality(
	ctx context.Context,
) (common.Checkpoint, error) {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(runningNodes, t.maxConsecutiveErrorsOnWaits)
	)

	for {
		select {
		case <-ctx.Done():
			return common.Checkpoint{}, ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(ctx context.Context, n *node.Node, r *result) {
					defer wg.Done()
					var (
						b             = n.BeaconClient
						finalizedFork string
					)

					headBlock, err := b.BlockV2(ctx, eth2api.BlockHead)
					if err != nil {
						r.err = errors.Wrap(err, "failed to poll head")
						return
					}
					slot := headBlock.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= t.spec.SLOTS_PER_EPOCH {
						r.fatal = fmt.Errorf(
							"unable to sync for an entire epoch: clockSlot=%d, slot=%d",
							clockSlot,
							slot,
						)
						return
					}

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					execution := ethcommon.Hash{}
					if exeuctionPayload, _, _, err := headBlock.ExecutionPayload(); err == nil {
						execution = exeuctionPayload.BlockHash
					}

					finalizedExecution := ethcommon.Hash{}
					if (checkpoints.Finalized != common.Checkpoint{}) {
						if finalizedBlock, err := b.BlockV2(
							ctx,
							eth2api.BlockIdRoot(checkpoints.Finalized.Root),
						); err != nil {
							r.err = errors.Wrap(
								err,
								"failed to retrieve block",
							)
							return
						} else {
							finalizedFork = finalizedBlock.Version
							if exeuctionPayload, _, _, err := finalizedBlock.ExecutionPayload(); err == nil {
								finalizedExecution = exeuctionPayload.BlockHash
							}
						}
					}

					r.msg = fmt.Sprintf(
						"fork=%s, finalized_fork=%s, clock_slot=%s, slot=%d, head=%s, "+
							"exec_payload=%s, finalized_exec_payload=%s, justified=%s, finalized=%s",
						headBlock.Version,
						finalizedFork,
						clockSlot,
						slot,
						utils.Shorten(headBlock.Root().String()),
						utils.Shorten(execution.Hex()),
						utils.Shorten(finalizedExecution.Hex()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
					)

					if !bytes.Equal(
						finalizedExecution[:],
						EMPTY_EXEC_HASH[:],
					) {
						r.done = true
						r.result = checkpoints.Finalized
					}
				}(
					ctx,
					n,
					results[i],
				)
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return common.Checkpoint{}, err
			}
			results.PrintMessages(t.Logf)
			if results.AllDone() {
				if cp, ok := results[0].result.(common.Checkpoint); ok {
					return cp, nil
				}
			}
		}
	}
}

// Waits for the current epoch to be finalized, or timeoutSlots have passed, whichever happens first.
func (t *Testnet) WaitForCurrentEpochFinalization(
	ctx context.Context,
) (common.Checkpoint, error) {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(
			t.spec.SECONDS_PER_SLOT,
		) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(
			runningNodes,
			t.maxConsecutiveErrorsOnWaits,
		)
		epochToBeFinalized = t.spec.SlotToEpoch(t.spec.TimeToSlot(
			common.Timestamp(time.Now().Unix()),
			t.GenesisTime(),
		))
	)

	for {
		select {
		case <-ctx.Done():
			return common.Checkpoint{}, ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				i := i
				wg.Add(1)
				go func(ctx context.Context, n *node.Node, r *result) {
					defer wg.Done()

					b := n.BeaconClient

					headInfo, err := b.BlockV2(ctx, eth2api.BlockHead)
					if err != nil {
						r.err = errors.Wrap(err, "failed to poll head")
						return
					}

					slot := headInfo.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= t.spec.SLOTS_PER_EPOCH {
						r.fatal = fmt.Errorf(
							"unable to sync for an entire epoch: clockSlot=%d, slot=%d",
							clockSlot,
							slot,
						)
						return
					}

					checkpoints, err := b.BlockFinalityCheckpoints(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(
							err,
							"failed to poll finality checkpoint",
						)
						return
					}

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%d, slot=%d, head=%s justified=%s, "+
							"finalized=%s, epoch_to_finalize=%d",
						headInfo.Version,
						clockSlot,
						slot,
						utils.Shorten(headInfo.Root().String()),
						utils.Shorten(checkpoints.CurrentJustified.String()),
						utils.Shorten(checkpoints.Finalized.String()),
						epochToBeFinalized,
					)

					if checkpoints.Finalized != (common.Checkpoint{}) &&
						checkpoints.Finalized.Epoch >= epochToBeFinalized {
						r.done = true
						r.result = checkpoints.Finalized
					}
				}(ctx, n, results[i])

			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return common.Checkpoint{}, err
			}
			results.PrintMessages(t.Logf)
			if results.AllDone() {
				t.Logf("INFO: Epoch %d finalized", epochToBeFinalized)
				if cp, ok := results[0].result.(common.Checkpoint); ok {
					return cp, nil
				}
			}
		}
	}
}

// Waits for any execution payload to be available included in a beacon block (merge),
// or timeoutSlots have passed, whichever happens first.
func (t *Testnet) WaitForExecutionPayload(
	ctx context.Context,
) (ethcommon.Hash, error) {
	var (
		genesis      = t.GenesisTimeUnix()
		slotDuration = time.Duration(t.spec.SECONDS_PER_SLOT) * time.Second
		timer        = time.NewTicker(slotDuration)
		runningNodes = t.VerificationNodes().Running()
		results      = makeResults(
			runningNodes,
			t.maxConsecutiveErrorsOnWaits,
		)
		executionClient = runningNodes[0].ExecutionClient
		ttdReached      = false
	)

	for {
		select {
		case <-ctx.Done():
			return ethcommon.Hash{}, ctx.Err()
		case tim := <-timer.C:
			// start polling after first slot of genesis
			if tim.Before(genesis.Add(slotDuration)) {
				t.Logf("Time till genesis: %s", genesis.Sub(tim))
				continue
			}

			if !ttdReached {
				// Check if TTD has been reached
				if td, err := executionClient.TotalDifficultyByNumber(ctx, nil); err == nil {
					if td.Cmp(
						t.executionGenesis.Genesis.Config.TerminalTotalDifficulty,
					) >= 0 {
						ttdReached = true
					} else {
						continue
					}
				} else {
					t.Logf("Error querying eth1 for TTD: %v", err)
				}
			}

			// new slot, log and check status of all beacon nodes
			var (
				wg        sync.WaitGroup
				clockSlot = t.spec.TimeToSlot(
					common.Timestamp(time.Now().Unix()),
					t.GenesisTime(),
				)
			)
			results.Clear()

			for i, n := range runningNodes {
				wg.Add(1)
				go func(ctx context.Context, n *node.Node, r *result) {
					defer wg.Done()

					b := n.BeaconClient

					versionedBlock, err := b.BlockV2(
						ctx,
						eth2api.BlockHead,
					)
					if err != nil {
						r.err = errors.Wrap(err, "failed to retrieve block")
						return
					}

					slot := versionedBlock.Slot()
					if clockSlot > slot &&
						(clockSlot-slot) >= t.spec.SLOTS_PER_EPOCH {
						r.fatal = fmt.Errorf(
							"unable to sync for an entire epoch: clockSlot=%d, slot=%d",
							clockSlot,
							slot,
						)
						return
					}

					executionHash := ethcommon.Hash{}
					if executionPayload, _, _, err := versionedBlock.ExecutionPayload(); err == nil {
						executionHash = executionPayload.BlockHash
					}

					health, _ := GetHealth(ctx, b, t.spec, slot)

					r.msg = fmt.Sprintf(
						"fork=%s, clock_slot=%d, slot=%d, "+
							"head=%s, health=%.2f, exec_payload=%s",
						versionedBlock.Version,
						clockSlot,
						slot,
						utils.Shorten(versionedBlock.Root().String()),
						health,
						utils.Shorten(executionHash.Hex()),
					)

					if !bytes.Equal(executionHash[:], EMPTY_EXEC_HASH[:]) {
						r.done = true
						r.result = executionHash
					}
				}(ctx, n, results[i])
			}
			wg.Wait()

			if err := results.CheckError(); err != nil {
				return ethcommon.Hash{}, err
			}
			results.PrintMessages(t.Logf)
			if results.AllDone() {
				if h, ok := results[0].result.(ethcommon.Hash); ok {
					return h, nil
				}
			}

		}
	}
}

func GetHealth(
	parentCtx context.Context,
	bn *beacon_client.BeaconClient,
	spec *common.Spec,
	slot common.Slot,
) (float64, error) {
	var health float64
	stateInfo, err := bn.BeaconStateV2(parentCtx, eth2api.StateIdSlot(slot))
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve state: %v", err)
	}
	currentEpochParticipation := stateInfo.CurrentEpochParticipation()
	if currentEpochParticipation != nil {
		// Altair and after
		health = calcHealth(currentEpochParticipation)
	} else {
		if stateInfo.Version != "phase0" {
			return 0, fmt.Errorf("calculate participation")
		}
		state := stateInfo.Data.(*phase0.BeaconState)
		epoch := spec.SlotToEpoch(slot)
		validatorIds := make([]eth2api.ValidatorId, 0, len(state.Validators))
		for id, validator := range state.Validators {
			if epoch >= validator.ActivationEligibilityEpoch &&
				epoch < validator.ExitEpoch &&
				!validator.Slashed {
				validatorIds = append(
					validatorIds,
					eth2api.ValidatorIdIndex(id),
				)
			}
		}
		var (
			beforeEpoch = 0
			afterEpoch  = spec.SlotToEpoch(slot)
		)

		// If it's genesis, keep before also set to 0.
		if afterEpoch != 0 {
			beforeEpoch = int(spec.SlotToEpoch(slot)) - 1
		}
		balancesBefore, err := bn.StateValidatorBalances(
			parentCtx,
			eth2api.StateIdSlot(beforeEpoch*int(spec.SLOTS_PER_EPOCH)),
			validatorIds,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"failed to retrieve validator balances: %v",
				err,
			)
		}
		balancesAfter, err := bn.StateValidatorBalances(
			parentCtx,
			eth2api.StateIdSlot(int(afterEpoch)*int(spec.SLOTS_PER_EPOCH)),
			validatorIds,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"failed to retrieve validator balances: %v",
				err,
			)
		}
		health = legacyCalcHealth(spec, balancesBefore, balancesAfter)
	}
	return health, nil
}

func calcHealth(p altair.ParticipationRegistry) float64 {
	sum := 0
	for _, p := range p {
		sum += int(p)
	}
	avg := float64(sum) / float64(len(p))
	return avg / float64(MAX_PARTICIPATION_SCORE)
}

// legacyCalcHealth calculates the health of the network based on balances at
// the beginning of an epoch versus the balances at the end.
//
// NOTE: this isn't strictly the most correct way of doing things, but it is
// quite accurate and doesn't require implementing the attestation processing
// logic here.
func legacyCalcHealth(
	spec *common.Spec,
	before, after []eth2api.ValidatorBalanceResponse,
) float64 {
	sum_before := big.NewInt(0)
	sum_after := big.NewInt(0)
	for i := range before {
		sum_before.Add(sum_before, big.NewInt(int64(before[i].Balance)))
		sum_after.Add(sum_after, big.NewInt(int64(after[i].Balance)))
	}
	count := big.NewInt(int64(len(before)))
	avg_before := big.NewInt(0).Div(sum_before, count).Uint64()
	avg_after := sum_after.Div(sum_after, count).Uint64()
	reward := avg_before * uint64(
		spec.BASE_REWARD_FACTOR,
	) / math.IntegerSquareRootPrysm(
		sum_before.Uint64(),
	) / uint64(
		spec.HYSTERESIS_QUOTIENT,
	)
	return float64(
		avg_after-avg_before,
	) / float64(
		reward*common.BASE_REWARDS_PER_EPOCH,
	)
}
