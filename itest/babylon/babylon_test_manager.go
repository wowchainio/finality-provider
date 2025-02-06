package e2etest_babylon

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	bbnclient "github.com/babylonlabs-io/babylon/client/client"
	"github.com/babylonlabs-io/babylon/testutil/datagen"
	bbntypes "github.com/babylonlabs-io/babylon/types"
	fpcc "github.com/babylonlabs-io/finality-provider/clientcontroller"
	ccapi "github.com/babylonlabs-io/finality-provider/clientcontroller/api"
	bbncc "github.com/babylonlabs-io/finality-provider/clientcontroller/babylon"
	"github.com/babylonlabs-io/finality-provider/eotsmanager/client"
	eotsconfig "github.com/babylonlabs-io/finality-provider/eotsmanager/config"
	fpcfg "github.com/babylonlabs-io/finality-provider/finality-provider/config"
	"github.com/babylonlabs-io/finality-provider/finality-provider/service"
	e2eutils "github.com/babylonlabs-io/finality-provider/itest"
	"github.com/babylonlabs-io/finality-provider/itest/container"
	base_test_manager "github.com/babylonlabs-io/finality-provider/itest/test-manager"
	"github.com/babylonlabs-io/finality-provider/testutil"
	"github.com/babylonlabs-io/finality-provider/types"
	"github.com/btcsuite/btcd/btcec/v2"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	eventuallyWaitTimeOut = 5 * time.Minute
	eventuallyPollTime    = 500 * time.Millisecond

	testMoniker = "test-moniker"
	testChainID = "chain-test"
	passphrase  = "testpass"
	hdPath      = ""
)

type TestManager struct {
	*base_test_manager.BaseTestManager
	EOTSServerHandler *e2eutils.EOTSServerHandler
	EOTSHomeDir       string
	FpConfig          *fpcfg.Config
	Fps               []*service.FinalityProviderApp
	EOTSClient        *client.EOTSManagerGRpcClient
	BBNConsumerClient *bbncc.BabylonConsumerController
	baseDir           string
	manager           *container.Manager
	logger            *zap.Logger
}

func StartManager(t *testing.T, ctx context.Context) *TestManager {
	testDir, err := base_test_manager.TempDir(t, "fp-e2e-test-*")
	require.NoError(t, err)

	loggerConfig := zap.NewDevelopmentConfig()
	loggerConfig.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	logger, err := loggerConfig.Build()
	require.NoError(t, err)

	// 1. generate covenant committee
	covenantQuorum := 2
	numCovenants := 3
	covenantPrivKeys, covenantPubKeys := e2eutils.GenerateCovenantCommittee(numCovenants, t)

	// 2. prepare Babylon node
	manager, err := container.NewManager(t)
	require.NoError(t, err)

	// Create temp dir for babylon node
	babylonDir, err := base_test_manager.TempDir(t, "babylon-test-*")
	require.NoError(t, err)

	// Start babylon node in docker
	babylond, err := manager.RunBabylondResource(t, babylonDir, covenantQuorum, covenantPubKeys)
	require.NoError(t, err)
	require.NotNil(t, babylond)

	keyDir := filepath.Join(babylonDir, "node0", "babylond")
	fpHomeDir := filepath.Join(testDir, "fp-home")
	cfg := e2eutils.DefaultFpConfig(keyDir, fpHomeDir)

	// update ports with the dynamically allocated ones from docker
	cfg.BabylonConfig.RPCAddr = fmt.Sprintf("http://localhost:%s", babylond.GetPort("26657/tcp"))
	cfg.BabylonConfig.GRPCAddr = fmt.Sprintf("https://localhost:%s", babylond.GetPort("9090/tcp"))

	var bc ccapi.ClientController
	var bcc ccapi.ConsumerController
	require.Eventually(t, func() bool {
		bbnCfg := fpcfg.BBNConfigToBabylonConfig(cfg.BabylonConfig)
		bbnCl, err := bbnclient.New(&bbnCfg, logger)
		if err != nil {
			t.Logf("failed to create Babylon client: %v", err)
			return false
		}
		bc, err = bbncc.NewBabylonController(bbnCl, cfg.BabylonConfig, &cfg.BTCNetParams, logger)
		if err != nil {
			t.Logf("failed to create Babylon controller: %v", err)
			return false
		}
		err = bc.Start()
		if err != nil {
			t.Logf("failed to start Babylon controller: %v", err)
			return false
		}
		bcc, err = bbncc.NewBabylonConsumerController(cfg.BabylonConfig, &cfg.BTCNetParams, logger)
		if err != nil {
			t.Logf("failed to create Babylon consumer controller: %v", err)
			return false
		}
		return true
	}, 5*time.Second, eventuallyPollTime)

	// Prepare EOTS manager
	eotsHomeDir := filepath.Join(testDir, "eots-home")
	eotsCfg := eotsconfig.DefaultConfigWithHomePath(eotsHomeDir)
	eotsCfg.RPCListener = fmt.Sprintf("127.0.0.1:%d", testutil.AllocateUniquePort(t))
	eotsCfg.Metrics.Port = testutil.AllocateUniquePort(t)
	eh := e2eutils.NewEOTSServerHandler(t, eotsCfg, eotsHomeDir)
	eh.Start(ctx)
	cfg.RPCListener = fmt.Sprintf("127.0.0.1:%d", testutil.AllocateUniquePort(t))
	eotsCli, err := client.NewEOTSManagerGRpcClient(eotsCfg.RPCListener)
	require.NoError(t, err)

	tm := &TestManager{
		BaseTestManager: &base_test_manager.BaseTestManager{
			BBNClient:        bc.(*bbncc.BabylonController),
			CovenantPrivKeys: covenantPrivKeys,
		},
		EOTSServerHandler: eh,
		EOTSHomeDir:       eotsHomeDir,
		FpConfig:          cfg,
		EOTSClient:        eotsCli,
		BBNConsumerClient: bcc.(*bbncc.BabylonConsumerController),
		baseDir:           testDir,
		manager:           manager,
		logger:            logger,
	}

	tm.WaitForServicesStart(t)

	return tm
}

func (tm *TestManager) AddFinalityProvider(t *testing.T, ctx context.Context) *service.FinalityProviderInstance {
	r := rand.New(rand.NewSource(time.Now().Unix()))

	// Create EOTS key
	eotsKeyName := fmt.Sprintf("eots-key-%s", datagen.GenRandomHexStr(r, 4))
	eotsPkBz, err := tm.EOTSClient.CreateKey(eotsKeyName, passphrase, hdPath)
	require.NoError(t, err)
	eotsPk, err := bbntypes.NewBIP340PubKey(eotsPkBz)
	require.NoError(t, err)

	t.Logf("the EOTS key is created: %s", eotsPk.MarshalHex())

	// Create FP babylon key
	fpKeyName := fmt.Sprintf("fp-key-%s", datagen.GenRandomHexStr(r, 4))
	fpHomeDir := filepath.Join(tm.baseDir, fmt.Sprintf("fp-%s", datagen.GenRandomHexStr(r, 4)))
	cfg := e2eutils.DefaultFpConfig(tm.baseDir, fpHomeDir)
	cfg.BabylonConfig.Key = fpKeyName
	cfg.BabylonConfig.RPCAddr = tm.FpConfig.BabylonConfig.RPCAddr
	cfg.BabylonConfig.GRPCAddr = tm.FpConfig.BabylonConfig.GRPCAddr
	fpBbnKeyInfo, err := testutil.CreateChainKey(cfg.BabylonConfig.KeyDirectory, cfg.BabylonConfig.ChainID, cfg.BabylonConfig.Key, cfg.BabylonConfig.KeyringBackend, passphrase, hdPath, "")
	require.NoError(t, err)

	t.Logf("the Babylon key is created: %s", fpBbnKeyInfo.AccAddress.String())

	// Add funds for new FP
	_, _, err = tm.manager.BabylondTxBankSend(t, fpBbnKeyInfo.AccAddress.String(), "1000000ubbn", "node0")
	require.NoError(t, err)

	// create new clients
	bc, err := fpcc.NewBabylonController(cfg, tm.logger)
	require.NoError(t, err)
	err = bc.Start()
	require.NoError(t, err)
	bcc, err := bbncc.NewBabylonConsumerController(cfg.BabylonConfig, &cfg.BTCNetParams, tm.logger)
	require.NoError(t, err)

	// Create and start finality provider app
	eotsCli, err := client.NewEOTSManagerGRpcClient(tm.EOTSServerHandler.Config().RPCListener)
	require.NoError(t, err)
	fpdb, err := cfg.DatabaseConfig.GetDBBackend()
	require.NoError(t, err)
	fpApp, err := service.NewFinalityProviderApp(cfg, bc, bcc, eotsCli, fpdb, tm.logger)
	require.NoError(t, err)
	err = fpApp.Start()
	require.NoError(t, err)

	// Create and register the finality provider
	commission := sdkmath.LegacyZeroDec()
	desc := newDescription(testMoniker)
	_, err = fpApp.CreateFinalityProvider(cfg.BabylonConfig.Key, testChainID, passphrase, eotsPk, desc, &commission)
	require.NoError(t, err)

	cfg.RPCListener = fmt.Sprintf("127.0.0.1:%d", testutil.AllocateUniquePort(t))
	cfg.Metrics.Port = testutil.AllocateUniquePort(t)

	err = fpApp.StartFinalityProvider(eotsPk, passphrase)
	require.NoError(t, err)

	fpServer := service.NewFinalityProviderServer(cfg, tm.logger, fpApp, fpdb)
	go func() {
		err = fpServer.RunUntilShutdown(ctx)
		require.NoError(t, err)
	}()

	tm.Fps = append(tm.Fps, fpApp)

	fpIns, err := fpApp.GetFinalityProviderInstance()
	require.NoError(t, err)

	return fpIns
}

func (tm *TestManager) WaitForServicesStart(t *testing.T) {
	require.Eventually(t, func() bool {
		_, err := tm.BBNClient.QueryBtcLightClientTip()

		return err == nil
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	t.Logf("Babylon node is started")
}

func StartManagerWithFinalityProvider(t *testing.T, n int, ctx context.Context) (*TestManager, []*service.FinalityProviderInstance) {
	tm := StartManager(t, ctx)

	var runningFps []*service.FinalityProviderInstance
	for i := 0; i < n; i++ {
		fpIns := tm.AddFinalityProvider(t, ctx)
		runningFps = append(runningFps, fpIns)
	}

	// Check finality providers on Babylon side
	require.Eventually(t, func() bool {
		fps, err := tm.BBNClient.QueryFinalityProviders()
		if err != nil {
			t.Logf("failed to query finality providers from Babylon %s", err.Error())
			return false
		}

		return len(fps) == n
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	t.Logf("the test manager is running with a finality provider")

	return tm, runningFps
}

func (tm *TestManager) Stop(t *testing.T) {
	for _, fpApp := range tm.Fps {
		err := fpApp.Stop()
		require.NoError(t, err)
	}
	err := tm.manager.ClearResources()
	require.NoError(t, err)
	err = os.RemoveAll(tm.baseDir)
	require.NoError(t, err)
}

func (tm *TestManager) CheckBlockFinalization(t *testing.T, height uint64, num int) {
	// We need to ensure votes are collected at the given height
	require.Eventually(t, func() bool {
		votes, err := tm.BBNClient.QueryVotesAtHeight(height)
		if err != nil {
			t.Logf("failed to get the votes at height %v: %s", height, err.Error())
			return false
		}
		return len(votes) == num
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	// As the votes have been collected, the block should be finalized
	require.Eventually(t, func() bool {
		finalized, err := tm.BBNConsumerClient.QueryIsBlockFinalized(height)
		if err != nil {
			t.Logf("failed to query block at height %v: %s", height, err.Error())
			return false
		}
		return finalized
	}, eventuallyWaitTimeOut, eventuallyPollTime)
}

func (tm *TestManager) WaitForFpVoteCast(t *testing.T, fpIns *service.FinalityProviderInstance) uint64 {
	var lastVotedHeight uint64
	require.Eventually(t, func() bool {
		if fpIns.GetLastVotedHeight() > 0 {
			lastVotedHeight = fpIns.GetLastVotedHeight()
			return true
		}
		return false
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	return lastVotedHeight
}

func (tm *TestManager) GetFpPrivKey(t *testing.T, fpPk []byte) *btcec.PrivateKey {
	record, err := tm.EOTSClient.KeyRecord(fpPk, passphrase)
	require.NoError(t, err)
	return record.PrivKey
}

func (tm *TestManager) StopAndRestartFpAfterNBlocks(t *testing.T, n int, fpIns *service.FinalityProviderInstance) {
	blockBeforeStop, err := tm.BBNConsumerClient.QueryLatestBlockHeight()
	require.NoError(t, err)
	err = fpIns.Stop()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		headerAfterStop, err := tm.BBNConsumerClient.QueryLatestBlockHeight()
		if err != nil {
			return false
		}

		return headerAfterStop >= uint64(n)+blockBeforeStop
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	t.Log("restarting the finality-provider instance")

	err = fpIns.Start()
	require.NoError(t, err)
}

func (tm *TestManager) WaitForNFinalizedBlocks(t *testing.T, n uint) *types.BlockInfo {
	var (
		firstFinalizedBlock *types.BlockInfo
		err                 error
		lastFinalizedBlock  *types.BlockInfo
	)

	require.Eventually(t, func() bool {
		lastFinalizedBlock, err = tm.BBNConsumerClient.QueryLatestFinalizedBlock()
		if err != nil {
			t.Logf("failed to get the latest finalized block: %s", err.Error())
			return false
		}
		if lastFinalizedBlock == nil {
			return false
		}
		if firstFinalizedBlock == nil {
			firstFinalizedBlock = lastFinalizedBlock
		}
		return lastFinalizedBlock.Height-firstFinalizedBlock.Height >= uint64(n-1)
	}, eventuallyWaitTimeOut, eventuallyPollTime)

	t.Logf("the block is finalized at %v", lastFinalizedBlock.Height)

	return lastFinalizedBlock
}

func newDescription(moniker string) *stakingtypes.Description {
	dec := stakingtypes.NewDescription(moniker, "", "", "", "")
	return &dec
}
