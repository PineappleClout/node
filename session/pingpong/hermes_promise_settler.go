/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package pingpong

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/mysteriumnetwork/node/config"
	nodevent "github.com/mysteriumnetwork/node/core/node/event"
	"github.com/mysteriumnetwork/node/core/service/servicestate"
	"github.com/mysteriumnetwork/node/eventbus"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/session/pingpong/event"
	"github.com/mysteriumnetwork/payments/bindings"
	"github.com/mysteriumnetwork/payments/client"
	"github.com/mysteriumnetwork/payments/crypto"
	"github.com/rs/zerolog/log"
)

type settlementHistoryStorage interface {
	Store(she SettlementHistoryEntry) error
}

type providerChannelStatusProvider interface {
	SubscribeToPromiseSettledEvent(chainID int64, providerID, hermesID common.Address) (sink chan *bindings.HermesImplementationPromiseSettled, cancel func(), err error)
	GetHermesFee(chainID int64, hermesAddress common.Address) (uint16, error)
	CalculateHermesFee(chainID int64, hermesAddress common.Address, value *big.Int) (*big.Int, error)
	GetMystBalance(chainID int64, mystAddress, identity common.Address) (*big.Int, error)
}

type paySettler interface {
	PayAndSettle(r []byte, em crypto.ExchangeMessage, providerID identity.Identity, sessionID string) <-chan error
}

type ks interface {
	Accounts() []accounts.Account
	SignHash(a accounts.Account, hash []byte) ([]byte, error)
}

type registrationStatusProvider interface {
	GetRegistrationStatus(chainID int64, id identity.Identity) (registry.RegistrationStatus, error)
}

type promiseStorage interface {
	Get(chainID int64, channelID string) (HermesPromise, error)
}

type transactor interface {
	SettleAndRebalance(hermesID, providerID string, promise crypto.Promise) error
	SettleWithBeneficiary(id, beneficiary, hermesID string, promise crypto.Promise) error
	PayAndSettle(hermesID, providerID string, promise crypto.Promise, beneficiary string, beneficiarySignature string) error
	SettleIntoStake(hermesID, providerID string, promise crypto.Promise) error
	FetchSettleFees(chainID int64) (registry.FeesResponse, error)
}

type hermesChannelProvider interface {
	Get(chainID int64, id identity.Identity, hermesID common.Address) (HermesChannel, bool)
	Fetch(chainID int64, id identity.Identity, hermesID common.Address) (HermesChannel, error)
}

type hermesCaller interface {
	UpdatePromiseFee(promise crypto.Promise, newFee *big.Int) (crypto.Promise, error)
}

type receivedPromise struct {
	provider    identity.Identity
	hermesID    common.Address
	promise     crypto.Promise
	beneficiary common.Address
}

// HermesPromiseSettler is responsible for settling the hermes promises.
type HermesPromiseSettler interface {
	ForceSettle(chainID int64, providerID identity.Identity, hermesID common.Address) error
	SettleWithBeneficiary(chainID int64, providerID identity.Identity, beneficiary, hermesID common.Address) error
	SettleIntoStake(chainID int64, providerID identity.Identity, hermesID common.Address) error
	GetHermesFee(chainID int64, hermesID common.Address) (uint16, error)
}

// hermesPromiseSettler is responsible for settling the hermes promises.
type hermesPromiseSettler struct {
	bc                         providerChannelStatusProvider
	config                     HermesPromiseSettlerConfig
	lock                       sync.RWMutex
	registrationStatusProvider registrationStatusProvider
	ks                         ks
	transactor                 transactor
	channelProvider            hermesChannelProvider
	settlementHistoryStorage   settlementHistoryStorage
	hermesURLGetter            hermesURLGetter
	hermesCallerFactory        HermesCallerFactory
	addressProvider            addressProvider
	paySettler                 paySettler
	promiseStorage             promiseStorage
	// TODO: Consider adding chain ID to this as well.
	currentState map[identity.Identity]settlementState
	settleQueue  chan receivedPromise
	stop         chan struct{}
	once         sync.Once

	rnd     *rand.Rand
	rndLock sync.Mutex
}

// HermesPromiseSettlerConfig configures the hermes promise settler accordingly.
type HermesPromiseSettlerConfig struct {
	Threshold            float64
	MaxWaitForSettlement time.Duration
	L1ChainID            int64
	L2ChainID            int64
}

// NewHermesPromiseSettler creates a new instance of hermes promise settler.
func NewHermesPromiseSettler(transactor transactor, promiseStorage promiseStorage, paySettler paySettler, addressProvider addressProvider, hermesCallerFactory HermesCallerFactory, hermesURLGetter hermesURLGetter, channelProvider hermesChannelProvider, providerChannelStatusProvider providerChannelStatusProvider, registrationStatusProvider registrationStatusProvider, ks ks, settlementHistoryStorage settlementHistoryStorage, config HermesPromiseSettlerConfig) *hermesPromiseSettler {
	return &hermesPromiseSettler{
		bc:                         providerChannelStatusProvider,
		ks:                         ks,
		registrationStatusProvider: registrationStatusProvider,
		config:                     config,
		currentState:               make(map[identity.Identity]settlementState),
		channelProvider:            channelProvider,
		settlementHistoryStorage:   settlementHistoryStorage,
		hermesCallerFactory:        hermesCallerFactory,
		hermesURLGetter:            hermesURLGetter,
		addressProvider:            addressProvider,
		promiseStorage:             promiseStorage,
		paySettler:                 paySettler,

		// defaulting to a queue of 5, in case we have a few active identities.
		settleQueue: make(chan receivedPromise, 5),
		stop:        make(chan struct{}),
		transactor:  transactor,
	}
}

// GetHermesFee fetches the hermes fee.
func (aps *hermesPromiseSettler) GetHermesFee(chainID int64, hermesID common.Address) (uint16, error) {
	return aps.bc.GetHermesFee(chainID, hermesID)
}

// loadInitialState loads the initial state for the given identity. Inteded to be called on service start.
func (aps *hermesPromiseSettler) loadInitialState(chainID int64, id identity.Identity) error {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if _, ok := aps.currentState[id]; ok {
		log.Info().Msgf("State for %v already loaded, skipping", id)
		return nil
	}

	status, err := aps.registrationStatusProvider.GetRegistrationStatus(chainID, id)
	if err != nil {
		return fmt.Errorf("could not check registration status for %v: %w", id, err)
	}

	if status != registry.Registered {
		log.Info().Msgf("Provider %v not registered, skipping", id)
		return nil
	}

	aps.currentState[id] = settlementState{
		registered: true,
	}
	return nil
}

// Subscribe subscribes the hermes promise settler to the appropriate events
func (aps *hermesPromiseSettler) Subscribe(bus eventbus.Subscriber) error {
	err := bus.SubscribeAsync(nodevent.AppTopicNode, aps.handleNodeEvent)
	if err != nil {
		return fmt.Errorf("could not subscribe to node status event: %w", err)
	}

	err = bus.SubscribeAsync(registry.AppTopicIdentityRegistration, aps.handleRegistrationEvent)
	if err != nil {
		return fmt.Errorf("could not subscribe to registration event: %w", err)
	}

	err = bus.SubscribeAsync(servicestate.AppTopicServiceStatus, aps.handleServiceEvent)
	if err != nil {
		return fmt.Errorf("could not subscribe to service status event: %w", err)
	}

	err = bus.SubscribeAsync(event.AppTopicSettlementRequest, aps.handleSettlementEvent)
	if err != nil {
		return fmt.Errorf("could not subscribe to settlement event: %w", err)
	}

	err = bus.SubscribeAsync(event.AppTopicHermesPromise, aps.handleHermesPromiseReceived)
	if err != nil {
		return fmt.Errorf("could not subscribe to hermes promise event: %w", err)
	}
	return nil
}

func (aps *hermesPromiseSettler) handleSettlementEvent(event event.AppEventSettlementRequest) {
	err := aps.ForceSettle(event.ChainID, event.ProviderID, event.HermesID)
	if err != nil {
		log.Error().Err(err).Msg("could not settle promise")
	}
}

func (aps *hermesPromiseSettler) chainID() int64 {
	return config.GetInt64(config.FlagChainID)
}

func (aps *hermesPromiseSettler) handleServiceEvent(event servicestate.AppEventServiceStatus) {
	switch event.Status {
	case string(servicestate.Running):
		err := aps.loadInitialState(aps.chainID(), identity.FromAddress(event.ProviderID))
		if err != nil {
			log.Error().Err(err).Msgf("could not load initial state for provider %v", event.ProviderID)
		}
	default:
		log.Debug().Msgf("Ignoring service event with status %v", event.Status)
	}
}

func (aps *hermesPromiseSettler) handleNodeEvent(payload nodevent.Payload) {
	if payload.Status == nodevent.StatusStarted {
		aps.handleNodeStart()
		return
	}

	if payload.Status == nodevent.StatusStopped {
		aps.handleNodeStop()
		return
	}
}

func (aps *hermesPromiseSettler) handleRegistrationEvent(payload registry.AppEventIdentityRegistration) {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if payload.Status != registry.Registered {
		log.Debug().Msgf("Ignoring event %v for provider %q", payload.Status.String(), payload.ID)
		return
	}
	log.Info().Msgf("Identity registration event received for provider %q", payload.ID)

	s := aps.currentState[payload.ID]
	s.registered = true
	aps.currentState[payload.ID] = s
	log.Info().Msgf("Identity registration event handled for provider %q", payload.ID)
}

func (aps *hermesPromiseSettler) handleHermesPromiseReceived(apep event.AppEventHermesPromise) {
	id := apep.ProviderID
	log.Info().Msgf("Received hermes promise for %q", id)
	aps.lock.Lock()
	defer aps.lock.Unlock()

	s, ok := aps.currentState[apep.ProviderID]
	if !ok {
		log.Error().Msgf("Have no info on provider %q, skipping", id)
		return
	}
	if !s.registered {
		log.Error().Msgf("provider %q not registered, skipping", id)
		return
	}

	var channel HermesChannel
	hc, ok := aps.channelProvider.Get(apep.Promise.ChainID, id, apep.HermesID)
	if ok {
		channel = hc
	} else {
		hc, err := aps.channelProvider.Fetch(apep.Promise.ChainID, id, apep.HermesID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Error().Err(err).Msgf("could not sync state for provider %v, hermesID %v", apep.ProviderID, apep.HermesID.Hex())
			return
		}
		channel = hc
	}

	log.Info().Msgf("Hermes %q promise state updated for provider %q", apep.HermesID.Hex(), id)

	if s.needsSettling(aps.config.Threshold, channel) {
		// TODO: when do we settle into stake? Do we ever auto settle into stake now?
		// if channel.channel.Stake != nil && channel.channel.StakeGoal != nil && channel.channel.Stake.Uint64() < channel.channel.StakeGoal.Uint64() {
		// 	go func() {
		// 		err := aps.SettleIntoStake(id, apep.HermesID)
		// 		log.Error().Err(err).Msgf("could not settle into stake for %q", apep.ProviderID)
		// 	}()
		// } else
		aps.initiateSettling(channel)
	}
}

func (aps *hermesPromiseSettler) initiateSettling(channel HermesChannel) {
	hexR, err := hex.DecodeString(channel.lastPromise.R)
	if err != nil {
		log.Error().Err(fmt.Errorf("could encode R: %w", err))
		return
	}
	channel.lastPromise.Promise.R = hexR

	aps.settleQueue <- receivedPromise{
		hermesID:    channel.HermesID,
		provider:    channel.Identity,
		promise:     channel.lastPromise.Promise,
		beneficiary: channel.Beneficiary,
	}
}

func (aps *hermesPromiseSettler) listenForSettlementRequests() {
	log.Info().Msg("Listening for settlement events")
	defer log.Info().Msg("Stopped listening for settlement events")

	for {
		select {
		case <-aps.stop:
			return
		case p := <-aps.settleQueue:
			channel, found := aps.channelProvider.Get(p.promise.ChainID, p.provider, p.hermesID)
			if !found {
				continue
			}
			go aps.settle(
				func(promise crypto.Promise) error {
					return aps.transactor.SettleAndRebalance(p.hermesID.Hex(), p.provider.Address, promise)
				},
				p.provider,
				p.hermesID,
				p.promise,
				p.beneficiary,
				channel.Channel.Settled,
			)
		}
	}
}

// SettleIntoStake settles the promise but transfers the money to stake increase, not to beneficiary.
func (aps *hermesPromiseSettler) SettleIntoStake(chainID int64, providerID identity.Identity, hermesID common.Address) error {
	channel, found := aps.channelProvider.Get(chainID, providerID, hermesID)
	if !found {
		return ErrNothingToSettle
	}

	hexR, err := hex.DecodeString(channel.lastPromise.R)
	if err != nil {
		return fmt.Errorf("could not decode R: %w", err)
	}
	channel.lastPromise.Promise.R = hexR
	return aps.settle(
		func(promise crypto.Promise) error {
			return aps.transactor.SettleIntoStake(hermesID.Hex(), providerID.Address, promise)
		},
		providerID,
		hermesID,
		channel.lastPromise.Promise,
		channel.Beneficiary,
		channel.Channel.Settled,
	)
}

// ErrNothingToSettle indicates that there is nothing to settle.
var ErrNothingToSettle = errors.New("nothing to settle for the given provider")

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) ForceSettle(chainID int64, providerID identity.Identity, hermesID common.Address) error {
	channel, found := aps.channelProvider.Get(chainID, providerID, hermesID)
	if !found {
		return ErrNothingToSettle
	}

	hexR, err := hex.DecodeString(channel.lastPromise.R)
	if err != nil {
		return fmt.Errorf("could not decode R: %w", err)
	}

	channel.lastPromise.Promise.R = hexR
	return aps.settle(
		func(promise crypto.Promise) error {
			return aps.transactor.SettleAndRebalance(hermesID.Hex(), providerID.Address, promise)
		},
		providerID,
		hermesID,
		channel.lastPromise.Promise,
		channel.Beneficiary,
		channel.Channel.Settled,
	)
}

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) SettleWithBeneficiary(chainID int64, providerID identity.Identity, beneficiary, hermesID common.Address) error {
	channel, found := aps.channelProvider.Get(chainID, providerID, hermesID)
	if !found {
		return ErrNothingToSettle
	}

	hexR, err := hex.DecodeString(channel.lastPromise.R)
	if err != nil {
		return fmt.Errorf("could not decode R: %w", err)
	}

	channel.lastPromise.Promise.R = hexR
	return aps.settle(
		func(promise crypto.Promise) error {
			return aps.transactor.SettleWithBeneficiary(providerID.Address, beneficiary.Hex(), hermesID.Hex(), promise)
		},
		providerID,
		hermesID,
		channel.lastPromise.Promise,
		beneficiary,
		channel.Channel.Settled,
	)
}

// ErrSettleTimeout indicates that the settlement has timed out
var ErrSettleTimeout = errors.New("settle timeout")

func (aps *hermesPromiseSettler) updatePromiseWithLatestFee(hermesID common.Address, promise crypto.Promise) (crypto.Promise, error) {
	log.Debug().Msg("Updating promise with latest fee")
	fees, err := aps.transactor.FetchSettleFees(promise.ChainID)
	if err != nil {
		return crypto.Promise{}, fmt.Errorf("could not fetch settle fees: %w", err)
	}

	hermesCaller, err := aps.getHermesCaller(promise.ChainID, hermesID)
	if err != nil {
		return crypto.Promise{}, fmt.Errorf("could not fetch settle fees: %w", err)
	}

	updatedPromise, err := hermesCaller.UpdatePromiseFee(promise, fees.Fee)
	if err != nil {
		return crypto.Promise{}, fmt.Errorf("could not update promise fee: %w", err)
	}
	updatedPromise.R = promise.R
	log.Debug().Msg("promise updated with latest fee")
	return updatedPromise, nil
}

func (aps *hermesPromiseSettler) withdraw(chainID int64, providerID identity.Identity, hermesID, beneficiary common.Address) error {
	if aps.isSettling(providerID) {
		return errors.New("provider already has settlement in progress")
	}

	aps.setSettling(providerID, true)
	log.Info().Msgf("Marked provider %v as requesting settlement", providerID)
	defer aps.setSettling(providerID, false)

	if chainID != aps.config.L2ChainID {
		return fmt.Errorf("can only withdraw from chain with ID %v, requested with %v", aps.config.L2ChainID, chainID)
	}

	consumerChannelAddress, err := aps.addressProvider.GetChannelAddress(chainID, providerID)
	if err != nil {
		return fmt.Errorf("could not generate channel address: %w", err)
	}

	hermesAddress, err := aps.addressProvider.GetActiveHermes(chainID)
	if err != nil {
		return fmt.Errorf("could not get active hermes address: %w", err)
	}

	// 1. calculate amount to withdraw - check balance on consumer channel
	amountToWithdraw, err := aps.getWithdrawalAmount(chainID, consumerChannelAddress)
	if err != nil {
		return err
	}

	// 2. issue a self promise
	msg, err := aps.issueSelfPromise(chainID, amountToWithdraw, providerID, consumerChannelAddress, hermesAddress)
	if err != nil {
		return err
	}

	// 3. call hermes with the promise via the payandsettle endpoint
	ch := aps.paySettler.PayAndSettle(msg.Promise.R, *msg, providerID, "")
	err = <-ch
	if err != nil {
		return fmt.Errorf("could not call hermes pay and settle:%w", err)
	}

	// 4. fetch the promise from storage
	chid, err := crypto.GenerateProviderChannelIDForPayAndSettle(providerID.Address, hermesAddress.Hex())
	if err != nil {
		return fmt.Errorf("could not get channel id for pay and settle: %w", err)
	}

	promise, err := aps.promiseStorage.Get(chainID, chid)
	if err != nil {
		return err
	}
	decodedR, err := hex.DecodeString(promise.R)
	if err != nil {
		return fmt.Errorf("could not decode R %w", err)
	}
	promise.Promise.R = decodedR

	// 5. add the missing beneficiary signature
	payload := crypto.NewPayAndSettleBeneficiaryPayload(beneficiary, aps.config.L1ChainID, chid, promise.Promise.Amount, client.ToBytes32(msg.Promise.R))
	err = payload.Sign(aps.ks, providerID.ToCommonAddress())
	if err != nil {
		return fmt.Errorf("could not sign pay and settle payload: %w", err)
	}

	// 6. call transactor to settle the promise.
	return aps.payAndSettle(
		func(promise crypto.Promise) error {
			return aps.transactor.PayAndSettle(hermesID.Hex(), providerID.Address, promise, payload.Beneficiary.Hex(), hex.EncodeToString(payload.Signature))
		},
		providerID,
		hermesID,
		promise.Promise,
		beneficiary,
		amountToWithdraw,
	)
}

func (aps *hermesPromiseSettler) payAndSettle(
	settleFunc func(promise crypto.Promise) error,
	provider identity.Identity,
	hermesID common.Address,
	promise crypto.Promise,
	beneficiary common.Address,
	withdrawalAmount *big.Int,
) error {
	updatedPromise, err := aps.updatePromiseWithLatestFee(hermesID, promise)
	if err != nil {
		log.Error().Err(err).Msg("Could not update promise fee")
		return err
	}

	if updatedPromise.Fee.Cmp(withdrawalAmount) > 0 {
		log.Error().Fields(map[string]interface{}{
			"promiseAmount": updatedPromise.Amount.String(),
			"transactorFee": updatedPromise.Fee.String(),
		}).Err(err).Msg("Earned amount too small for withdrawal")
		return fmt.Errorf("Amount too small for withdrawal. Need at least %v, have %v", updatedPromise.Fee.String(), withdrawalAmount.String())
	}

	sink, cancel, err := aps.bc.SubscribeToPromiseSettledEvent(promise.ChainID, provider.ToCommonAddress(), hermesID)
	if err != nil {
		log.Error().Err(err).Msg("Could not subscribe to promise settlement")
		return err
	}

	errCh := make(chan error)
	go func() {
		defer cancel()
		defer close(errCh)
		select {
		case <-aps.stop:
			return
		case info, more := <-sink:
			if !more || info == nil {
				break
			}
			log.Info().Msgf("Settling complete for provider %v", provider)
			return
		case <-time.After(aps.config.MaxWaitForSettlement):
			log.Info().Msgf("Settle timeout for %v", provider)

			// send a signal to waiter that the settlement has timed out
			errCh <- ErrSettleTimeout
			return
		}
	}()

	err = settleFunc(updatedPromise)
	if err != nil {
		cancel()
		log.Error().Err(err).Msgf("Could not settle promise for %v", provider)
		return err
	}

	return <-errCh
}

func (aps *hermesPromiseSettler) issueSelfPromise(chainID int64, amount *big.Int, providerID identity.Identity, consumerChannelAddress, hermesAddress common.Address) (*crypto.ExchangeMessage, error) {
	r := aps.generateR()
	agreementID := aps.generateAgreementID()
	invoice := crypto.CreateInvoice(agreementID, amount, big.NewInt(0), r, 1)
	invoice.Provider = providerID.ToCommonAddress().Hex()

	promise, err := crypto.CreatePromise(consumerChannelAddress.Hex(), chainID, amount, big.NewInt(0), invoice.Hashlock, aps.ks, providerID.ToCommonAddress())
	if err != nil {
		return nil, fmt.Errorf("could not create promise: %w", err)
	}

	promise.R = r

	msg, err := crypto.CreateExchangeMessageWithPromise(chainID, invoice, promise, hermesAddress.Hex(), aps.ks, providerID.ToCommonAddress())
	if err != nil {
		return nil, fmt.Errorf("could not get create exchange message: %w", err)
	}

	return msg, nil
}

func (aps *hermesPromiseSettler) generateR() []byte {
	aps.rndLock.Lock()
	defer aps.rndLock.Unlock()

	r := make([]byte, 32)
	aps.rnd.Read(r)
	return r
}

func (aps *hermesPromiseSettler) generateAgreementID() *big.Int {
	aps.rndLock.Lock()
	defer aps.rndLock.Unlock()

	agreementID := make([]byte, 32)
	aps.rnd.Read(agreementID)
	return new(big.Int).SetBytes(agreementID)
}

func (aps *hermesPromiseSettler) getWithdrawalAmount(chainID int64, consumerChannelAddress common.Address) (*big.Int, error) {
	mystAddr, err := aps.addressProvider.GetMystAddress(chainID)
	if err != nil {
		return nil, fmt.Errorf("could not get myst address: %w", err)
	}

	balance, err := aps.bc.GetMystBalance(chainID, mystAddr, consumerChannelAddress)
	if err != nil {
		return nil, fmt.Errorf("could not get myst balance: %w", err)
	}

	if balance.Cmp(big.NewInt(0)) <= 0 {
		return nil, fmt.Errorf("Nothing to withdraw. Balance in channel %v is %v", consumerChannelAddress.Hex(), balance)
	}

	return balance, nil
}

func (aps *hermesPromiseSettler) settle(
	settleFunc func(promise crypto.Promise) error,
	provider identity.Identity,
	hermesID common.Address,
	promise crypto.Promise,
	beneficiary common.Address,
	settled *big.Int,
) error {
	if aps.isSettling(provider) {
		return errors.New("provider already has settlement in progress")
	}

	aps.setSettling(provider, true)
	log.Info().Msgf("Marked provider %v as requesting settlement", provider)

	updatedPromise, err := aps.updatePromiseWithLatestFee(hermesID, promise)
	if err != nil {
		aps.setSettling(provider, false)
		log.Error().Err(err).Msg("Could not update promise fee")
		return err
	}

	if settled == nil {
		settled = new(big.Int)
	}

	amountToSettle := new(big.Int).Sub(updatedPromise.Amount, settled)

	fee, err := aps.bc.CalculateHermesFee(promise.ChainID, hermesID, amountToSettle)
	if err != nil {
		aps.setSettling(provider, false)
		log.Error().Err(err).Msg("Could not calculate hermes fee")
		return err
	}

	totalFees := new(big.Int).Add(fee, updatedPromise.Fee)
	if totalFees.Cmp(amountToSettle) > 0 {
		aps.setSettling(provider, false)
		log.Error().Fields(map[string]interface{}{
			"amountToSettle": amountToSettle.String(),
			"promiseAmount":  updatedPromise.Amount.String(),
			"settled":        settled.String(),
			"transactorFee":  updatedPromise.Fee.String(),
			"hermesFee":      fee.String(),
			"totalFees":      totalFees.String(),
		}).Err(err).Msg("Earned amount too small for settling")
		return fmt.Errorf("Settlement fees exceed earning amount. Please provide more service and try again. Current earnings: %v, current fees: %v", amountToSettle, totalFees)
	}

	sink, cancel, err := aps.bc.SubscribeToPromiseSettledEvent(promise.ChainID, provider.ToCommonAddress(), hermesID)
	if err != nil {
		aps.setSettling(provider, false)
		log.Error().Err(err).Msg("Could not subscribe to promise settlement")
		return err
	}

	errCh := make(chan error)
	go func() {
		defer cancel()
		defer aps.setSettling(provider, false)
		defer close(errCh)
		select {
		case <-aps.stop:
			return
		case info, more := <-sink:
			if !more || info == nil {
				break
			}

			log.Info().Msgf("Settling complete for provider %v", provider)

			channelID, err := crypto.GenerateProviderChannelID(provider.Address, hermesID.Hex())
			if err != nil {
				log.Error().Err(err).Msg("Could not generate provider channel address")
			}

			ch, err := aps.channelProvider.Fetch(promise.ChainID, provider, hermesID)
			if err != nil {
				log.Error().Err(err).Msgf("Resync failed for provider %v", provider)
			} else {
				log.Info().Msgf("Resync success for provider %v", provider)
			}

			she := SettlementHistoryEntry{
				TxHash:         info.Raw.TxHash,
				ProviderID:     provider,
				HermesID:       hermesID,
				ChannelAddress: common.HexToAddress(channelID),
				Time:           time.Now().UTC(),
				Promise:        updatedPromise,
				Beneficiary:    beneficiary,
				Amount:         info.AmountSentToBeneficiary,
				Fees:           info.Fees,
				TotalSettled:   ch.Channel.Settled,
			}

			err = aps.settlementHistoryStorage.Store(she)
			if err != nil {
				log.Error().Err(err).Msg("Could not store settlement history")
			}

			return
		case <-time.After(aps.config.MaxWaitForSettlement):
			log.Info().Msgf("Settle timeout for %v", provider)

			// send a signal to waiter that the settlement has timed out
			errCh <- ErrSettleTimeout
			return
		}
	}()

	err = settleFunc(updatedPromise)
	if err != nil {
		cancel()
		log.Error().Err(err).Msgf("Could not settle promise for %v", provider)
		return err
	}

	return <-errCh
}

func (aps *hermesPromiseSettler) isSettling(id identity.Identity) bool {
	aps.lock.RLock()
	defer aps.lock.RUnlock()
	v, ok := aps.currentState[id]
	if !ok {
		return false
	}

	return v.settleInProgress
}

func (aps *hermesPromiseSettler) setSettling(id identity.Identity, settling bool) {
	aps.lock.Lock()
	defer aps.lock.Unlock()
	v := aps.currentState[id]
	v.settleInProgress = settling
	aps.currentState[id] = v
}

func (aps *hermesPromiseSettler) handleNodeStart() {
	go aps.listenForSettlementRequests()

	for _, v := range aps.ks.Accounts() {
		addr := identity.FromAddress(v.Address.Hex())
		go func(address identity.Identity) {
			err := aps.loadInitialState(aps.chainID(), address)
			if err != nil {
				log.Error().Err(err).Msgf("could not load initial state for %v", addr)
			}
		}(addr)
	}
}

func (aps *hermesPromiseSettler) getHermesCaller(chainID int64, hermesID common.Address) (HermesHTTPRequester, error) {
	addr, err := aps.hermesURLGetter.GetHermesURL(chainID, hermesID)
	if err != nil {
		return nil, fmt.Errorf("could not get hermes URL: %w", err)
	}
	return aps.hermesCallerFactory(addr), nil
}

func (aps *hermesPromiseSettler) handleNodeStop() {
	aps.once.Do(func() {
		close(aps.stop)
	})
}

// settlementState earning calculations model
type settlementState struct {
	settleInProgress bool
	registered       bool
}

func (ss settlementState) needsSettling(threshold float64, channel HermesChannel) bool {
	if !ss.registered {
		return false
	}

	if ss.settleInProgress {
		return false
	}

	if channel.Channel.Stake.Cmp(big.NewInt(0)) == 0 {
		// if starting with zero stake, only settle one myst or more.
		if channel.UnsettledBalance().Cmp(big.NewInt(0).SetUint64(crypto.Myst)) == -1 {
			return false
		}
	}

	floated := new(big.Float).SetInt(channel.availableBalance())
	calculatedThreshold := new(big.Float).Mul(big.NewFloat(threshold), floated)
	possibleEarnings := channel.UnsettledBalance()
	i, _ := calculatedThreshold.Int(nil)
	if possibleEarnings.Cmp(i) == -1 {
		return false
	}

	if channel.balance().Cmp(i) <= 0 {
		return true
	}

	return false
}
