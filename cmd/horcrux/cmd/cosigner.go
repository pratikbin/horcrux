package cmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/spf13/cobra"
	"github.com/strangelove-ventures/horcrux/signer"
	tmlog "github.com/tendermint/tendermint/libs/log"
	tmService "github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/types"
)

func init() {
	cosignerCmd.AddCommand(StartCosignerCmd())
	cosignerCmd.AddCommand(AddressCmd())
	rootCmd.AddCommand(cosignerCmd)
}

var cosignerCmd = &cobra.Command{
	Use:   "cosigner",
	Short: "Threshold mpc signer for TM based nodes",
}

type AddressCmdOutput struct {
	HexAddress        string
	PubKey            string
	ValConsAddress    string
	ValConsPubAddress string
}

func AddressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "address [bech32]",
		Short:        "Get public key hex address and valcons address",
		Example:      `horcrux cosigner address cosmos`,
		SilenceUsage: true,
		Args:         cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			err = validateCosignerConfig(config.Config)
			if err != nil {
				return
			}

			key, err := signer.LoadCosignerKey(config.keyFilePath(true))
			if err != nil {
				return fmt.Errorf("error reading cosigner key: %s", err)
			}

			pubKey := key.PubKey
			pubKeyAddress := pubKey.Address()

			pubKeyJSON, err := signer.PubKey("", pubKey)
			if err != nil {
				return err
			}

			output := AddressCmdOutput{
				HexAddress: strings.ToUpper(hex.EncodeToString(pubKeyAddress)),
				PubKey:     pubKeyJSON,
			}

			if len(args) == 1 {
				bech32ValConsAddress, err := bech32.ConvertAndEncode(args[0]+"valcons", pubKeyAddress)
				if err != nil {
					return err
				}
				output.ValConsAddress = bech32ValConsAddress
				pubKeyBech32, err := signer.PubKey(args[0], pubKey)
				if err != nil {
					return err
				}
				output.ValConsPubAddress = pubKeyBech32
			} else {
				bech32Hint := "Pass bech32 base prefix as argument to generate (e.g. cosmos)"
				output.ValConsAddress = bech32Hint
				output.ValConsPubAddress = bech32Hint
			}

			jsonOut, err := json.Marshal(output)
			if err != nil {
				return err
			}

			fmt.Println(string(jsonOut))

			return nil
		},
	}

	return cmd
}

func StartCosignerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start cosigner process",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if err = signer.RequireNotRunning(config.PidFile); err != nil {
				return err
			}

			err = validateCosignerConfig(config.Config)
			if err != nil {
				return err
			}

			var (
				// services to stop on shutdown
				services []tmService.Service
				pv       types.PrivValidator
				chainID  = config.Config.ChainID
				logger   = tmlog.NewTMLogger(tmlog.NewSyncWriter(os.Stdout)).With("module", "validator")
				cfg      signer.Config
			)

			cfg = signer.Config{
				Mode:              "mpc",
				PrivValKeyFile:    config.keyFilePath(true),
				PrivValStateDir:   config.StateDir,
				ChainID:           config.Config.ChainID,
				CosignerThreshold: config.Config.CosignerConfig.Threshold,
				ListenAddress:     config.Config.CosignerConfig.P2PListen,
				Nodes:             config.Config.Nodes(),
				Cosigners:         config.Config.CosignerPeers(),
			}

			if err = cfg.KeyFileExists(); err != nil {
				return err
			}

			logger.Info("Tendermint Validator", "mode", cfg.Mode,
				"priv-key", cfg.PrivValKeyFile, "priv-state-dir", cfg.PrivValStateDir)

			var val types.PrivValidator

			key, err := signer.LoadCosignerKey(cfg.PrivValKeyFile)
			if err != nil {
				return fmt.Errorf("error reading cosigner key: %s", err)
			}

			// ok to auto initialize on disk since the cosigner share is the one that actually
			// protects against double sign - this exists as a cache for the final signature
			signState, err := signer.LoadOrCreateSignState(config.privValStateFile(chainID))
			if err != nil {
				panic(err)
			}

			// state for our cosigner share
			// Not automatically initialized on disk to avoid double sign risk
			shareSignState, err := signer.LoadSignState(config.shareStateFile(chainID))
			if err != nil {
				panic(err)
			}

			cosigners := []signer.Cosigner{}

			// add ourselves as a peer so localcosigner can handle GetEphSecPart requests
			peers := []signer.CosignerPeer{{
				ID:        key.ID,
				PublicKey: key.RSAKey.PublicKey,
			}}

			for _, cosignerConfig := range cfg.Cosigners {
				cosigner := signer.NewRemoteCosigner(cosignerConfig.ID, cosignerConfig.Address)
				cosigners = append(cosigners, cosigner)

				if cosignerConfig.ID < 1 || cosignerConfig.ID > len(key.CosignerKeys) {
					log.Fatalf("Unexpected cosigner ID %d", cosignerConfig.ID)
				}

				pubKey := key.CosignerKeys[cosignerConfig.ID-1]
				peers = append(peers, signer.CosignerPeer{
					ID:        cosigner.GetID(),
					PublicKey: *pubKey,
				})
			}

			total := len(cfg.Cosigners) + 1
			localCosignerConfig := signer.LocalCosignerConfig{
				CosignerKey: key,
				SignState:   &shareSignState,
				RsaKey:      key.RSAKey,
				Address:     cfg.ListenAddress,
				Peers:       peers,
				Total:       uint8(total),
				Threshold:   uint8(cfg.CosignerThreshold),
			}

			localCosigner := signer.NewLocalCosigner(localCosignerConfig)

			timeout, err := time.ParseDuration(config.Config.CosignerConfig.Timeout)
			if err != nil {
				log.Fatalf("Error parsing configured timeout: %s. %v\n", config.Config.CosignerConfig.Timeout, err)
			}

			raftDir := filepath.Join(config.HomeDir, "raft")
			if err := os.MkdirAll(raftDir, 0700); err != nil {
				log.Fatalf("Error creating raft directory: %v\n", err)
			}

			// RAFT node ID is the cosigner ID
			nodeID := fmt.Sprint(key.ID)

			// Start RAFT store listener
			raftStore := signer.NewRaftStore(nodeID,
				raftDir, cfg.ListenAddress, timeout, logger, localCosigner, cosigners)
			if err := raftStore.Start(); err != nil {
				log.Fatalf("Error starting raft store: %v\n", err)
			}
			services = append(services, raftStore)

			val = signer.NewThresholdValidator(&signer.ThresholdValidatorOpt{
				Pubkey:    key.PubKey,
				Threshold: cfg.CosignerThreshold,
				SignState: signState,
				Cosigner:  localCosigner,
				Peers:     cosigners,
				RaftStore: raftStore,
				Logger:    logger,
			})

			raftStore.SetThresholdValidator(val.(*signer.ThresholdValidator))

			pv = &signer.PvGuard{PrivValidator: val}

			pubkey, err := pv.GetPubKey()
			if err != nil {
				log.Fatal(err)
			}
			logger.Info("Signer", "address", pubkey.Address())

			services, err = signer.StartRemoteSigners(services, logger, cfg.ChainID, pv, cfg.Nodes)
			if err != nil {
				panic(err)
			}

			signer.WaitAndTerminate(logger, services, config.PidFile)

			return nil
		},
	}

	return cmd
}
