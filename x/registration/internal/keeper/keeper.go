package keeper

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/x/auth/legacy/legacytx"
	"github.com/scrtlabs/SecretNetwork/x/registration/internal/types"
	ra "github.com/scrtlabs/SecretNetwork/x/registration/remote_attestation"
)

// Keeper will have a reference to Wasmer with it's own data directory.
type Keeper struct {
	storeKey sdk.StoreKey
	cdc      codec.BinaryCodec
	enclave  EnclaveInterface
	router   sdk.Router
}

// NewKeeper creates a new contract Keeper instance
func NewKeeper(cdc codec.BinaryCodec, storeKey sdk.StoreKey, router sdk.Router, enclave EnclaveInterface, homeDir string, bootstrap bool) Keeper {
	if !bootstrap {
		InitializeNode(homeDir, enclave)
	}

	return Keeper{
		storeKey: storeKey,
		cdc:      cdc,
		router:   router,
		enclave:  enclave,
	}
}

func InitializeNode(homeDir string, enclave EnclaveInterface) {
	seedPath := filepath.Join(homeDir, types.SecretNodeCfgFolder, types.SecretNodeSeedLegacyConfig)
	apiKey, err := types.GetApiKey()
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}

	if !fileExists(seedPath) {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, fmt.Sprintf("Searching for Seed configuration in path: %s was not found. Did you initialize the node?", seedPath)))
	}

	// get PK from CLI
	// get encrypted master key
	byteValue, err := getFile(seedPath)
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}

	var seedCfg types.SeedConfig
	var legacySeedCfg types.LegacySeedConfig
	err = json.Unmarshal(byteValue, &legacySeedCfg)
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}

	if len(legacySeedCfg.MasterCert) != 0 {
		cert, _, err := legacySeedCfg.Decode()
		if err != nil {
			panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
		}

		seedCfg.MasterKey, err = fetchPubKeyFromLegacyCert(cert)
		if err != nil {
			panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
		}

		seedCfg.EncryptedKey = legacySeedCfg.EncryptedKey
	} else {
		err = json.Unmarshal(byteValue, &seedCfg)

		if err != nil {
			panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
		}
	}

	pk, enc, err := seedCfg.Decode()
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}

	newEnc := make([]byte, len(enc)+1)

	tmp := make([]byte, 2)
	binary.LittleEndian.PutUint16(tmp, uint16(len(enc)))
	newEnc[0] = tmp[0]

	copy(newEnc[1:], enc)

	seedCfg.EncryptedKey = hex.EncodeToString(newEnc)

	err = validateSeedParams(seedCfg)
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}

	_, err = enclave.LoadSeed(pk, newEnc, apiKey)
	if err != nil {
		panic(sdkerrors.Wrap(types.ErrSeedInitFailed, err.Error()))
	}
}

func (k Keeper) RegisterNode(ctx sdk.Context, certificate ra.Certificate) ([]byte, error) {
	// fmt.Println("RegisterNode")
	var encSeed []byte

	if isSimulationMode(ctx) {
		// any sha256 hash is good enough
		encSeed = make([]byte, 32)
	} else {

		publicKey, err := ra.VerifyRaCert(certificate)
		if err != nil {
			return nil, sdkerrors.Wrap(types.ErrAuthenticateFailed, err.Error())
		}

		isAuth, err := k.isNodeAuthenticated(ctx, publicKey)
		if err != nil {
			return nil, sdkerrors.Wrap(types.ErrAuthenticateFailed, err.Error())
		}
		if isAuth {
			return k.getRegistrationInfo(ctx, publicKey).EncryptedSeed, nil
		}
		encSeed, err = k.enclave.GetEncryptedSeed(certificate)
		if err != nil {
			// return 0, sdkerrors.Wrap(err, "cosmwasm create")
			return nil, sdkerrors.Wrap(types.ErrAuthenticateFailed, err.Error())
		}
	}
	fmt.Println("Done RegisterNode")
	fmt.Println("Got seed: ", hex.EncodeToString(encSeed))
	regInfo := types.RegistrationNodeInfo{
		Certificate:   certificate,
		EncryptedSeed: encSeed,
	}
	k.SetRegistrationInfo(ctx, regInfo)

	return encSeed, nil
}

// returns true when simulation mode used by gas=auto queries
func isSimulationMode(ctx sdk.Context) bool {
	return ctx.GasMeter().Limit() == 0 && ctx.BlockHeight() != 0
}

func (k Keeper) handleSdkMessage(ctx sdk.Context, contractAddr sdk.Address, msg sdk.Msg) error {
	// make sure this account can send it
	for _, acct := range msg.GetSigners() {
		if !acct.Equals(contractAddr) {
			return sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "contract doesn't have permission")
		}
	}

	var res *sdk.Result
	var err error
	// find the handler and execute it
	if legacyMsg, ok := msg.(legacytx.LegacyMsg); ok {
		h := k.router.Route(ctx, legacyMsg.Route())
		if h == nil {
			return sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, legacyMsg.Route())
		}
		res, err = h(ctx, msg)
		if err != nil {
			return err
		}
	}

	events := make(sdk.Events, len(res.Events))
	for i := range res.Events {
		events[i] = sdk.Event(res.Events[i])
	}

	// redispatch all events, (type sdk.EventTypeMessage will be filtered out in the handler)
	ctx.EventManager().EmitEvents(events)

	return nil
}

func fetchPubKeyFromLegacyCert(cert []byte) (string, error) {
	pk, err := FetchRawPubKeyFromLegacyCert(cert)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(pk), nil
}

func FetchRawPubKeyFromLegacyCert(cert []byte) ([]byte, error) {
	pk, err := ra.VerifyRaCert(cert)
	if err != nil {
		return nil, err
	}

	return pk, nil
}

func validateSeedParams(config types.SeedConfig) error {
	lenKey := len(config.EncryptedKey) - 2

	if (lenKey != types.EncryptedKeyLength && lenKey != types.LegacyEncryptedKeyLength) || !IsHexString(config.EncryptedKey) {
		return sdkerrors.Wrap(types.ErrSeedValidationParams, "Invalid parameter: `seed` in seed parameters. Did you initialize the node?")
	}
	return nil
}

func IsHexString(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}
