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
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/mysteriumnetwork/node/eventbus"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/services/openvpn/discovery/dto"
	"github.com/mysteriumnetwork/payments/crypto"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// ErrConsumerPromiseValidationFailed represents an error where consumer tries to cheat us with incorrect promises.
var ErrConsumerPromiseValidationFailed = errors.New("consumer failed to issue promise for the correct amount")

// ErrAccountantFeeTooLarge indicates that we do not allow accountants with such high fees
var ErrAccountantFeeTooLarge = errors.New("accountants fee exceeds")

// PeerInvoiceSender allows to send invoices.
type PeerInvoiceSender interface {
	Send(crypto.Invoice) error
}

type feeProvider interface {
	FetchSettleFees() (registry.FeesResponse, error)
}

type bcHelper interface {
	GetAccountantFee(accountantAddress common.Address) (uint16, error)
	IsRegistered(registryAddress, addressToCheck common.Address) (bool, error)
}

type providerInvoiceStorage interface {
	Get(providerIdentity, consumerIdentity identity.Identity) (crypto.Invoice, error)
	Store(providerIdentity, consumerIdentity identity.Identity, invoice crypto.Invoice) error
	GetNewAgreementID(providerIdentity identity.Identity) (uint64, error)
	StoreR(providerIdentity identity.Identity, agreementID uint64, r string) error
	GetR(providerID identity.Identity, agreementID uint64) (string, error)
}

type accountantPromiseStorage interface {
	Store(providerID, accountantID identity.Identity, promise AccountantPromise) error
	Get(providerID, accountantID identity.Identity) (AccountantPromise, error)
}

type accountantCaller interface {
	RequestPromise(em crypto.ExchangeMessage) (crypto.Promise, error)
	RevealR(r string, provider string, agreementID uint64) error
}

// ErrExchangeWaitTimeout indicates that we did not get an exchange message in time.
var ErrExchangeWaitTimeout = errors.New("did not get a new exchange message")

// ErrExchangeValidationFailed indicates that there was an error with the exchange signature.
var ErrExchangeValidationFailed = errors.New("exchange validation failed")

// ErrConsumerNotRegistered represents the error that the consumer is not registered
var ErrConsumerNotRegistered = errors.New("consumer not registered")

const chargePeriodLeeway = time.Hour * 2

type lastInvoice struct {
	invoice crypto.Invoice
	r       []byte
}

// InvoiceTracker keeps tab of invoices and sends them to the consumer.
type InvoiceTracker struct {
	peer                            identity.Identity
	stop                            chan struct{}
	peerInvoiceSender               PeerInvoiceSender
	exchangeMessageChan             chan crypto.ExchangeMessage
	chargePeriod                    time.Duration
	exchangeMessageWaitTimeout      time.Duration
	accountantFailureCount          uint64
	notReceivedExchangeMessageCount uint64
	maxNotReceivedExchangeMessages  uint64
	once                            sync.Once
	invoiceStorage                  providerInvoiceStorage
	accountantPromiseStorage        accountantPromiseStorage
	timeTracker                     timeTracker
	paymentInfo                     dto.PaymentRate
	providerID                      identity.Identity
	accountantID                    identity.Identity
	lastInvoice                     lastInvoice
	lastExchangeMessage             crypto.ExchangeMessage
	accountantCaller                accountantCaller
	registryAddress                 string
	maxAccountantFailureCount       uint64
	maxAllowedAccountantFee         uint16
	bcHelper                        bcHelper
	publisher                       eventbus.Publisher
	feeProvider                     feeProvider
	transactorFee                   uint64
	maxRRecoveryLength              uint64
	channelAddressCalculator        channelAddressCalculator
}

// InvoiceTrackerDeps contains all the deps needed for invoice tracker.
type InvoiceTrackerDeps struct {
	Peer                       identity.Identity
	PeerInvoiceSender          PeerInvoiceSender
	InvoiceStorage             providerInvoiceStorage
	TimeTracker                timeTracker
	ChargePeriod               time.Duration
	ExchangeMessageChan        chan crypto.ExchangeMessage
	ExchangeMessageWaitTimeout time.Duration
	PaymentInfo                dto.PaymentRate
	ProviderID                 identity.Identity
	AccountantID               identity.Identity
	AccountantCaller           accountantCaller
	AccountantPromiseStorage   accountantPromiseStorage
	Registry                   string
	MaxAccountantFailureCount  uint64
	MaxRRecoveryLength         uint64
	MaxAllowedAccountantFee    uint16
	BlockchainHelper           bcHelper
	Publisher                  eventbus.Publisher
	FeeProvider                feeProvider
	ChannelAddressCalculator   channelAddressCalculator
}

// NewInvoiceTracker creates a new instance of invoice tracker.
func NewInvoiceTracker(
	itd InvoiceTrackerDeps) *InvoiceTracker {
	return &InvoiceTracker{
		peer:                           itd.Peer,
		stop:                           make(chan struct{}),
		peerInvoiceSender:              itd.PeerInvoiceSender,
		exchangeMessageChan:            itd.ExchangeMessageChan,
		exchangeMessageWaitTimeout:     itd.ExchangeMessageWaitTimeout,
		chargePeriod:                   itd.ChargePeriod,
		invoiceStorage:                 itd.InvoiceStorage,
		timeTracker:                    itd.TimeTracker,
		paymentInfo:                    itd.PaymentInfo,
		providerID:                     itd.ProviderID,
		accountantCaller:               itd.AccountantCaller,
		accountantPromiseStorage:       itd.AccountantPromiseStorage,
		accountantID:                   itd.AccountantID,
		maxNotReceivedExchangeMessages: calculateMaxNotReceivedExchangeMessageCount(chargePeriodLeeway, itd.ChargePeriod),
		maxAccountantFailureCount:      itd.MaxAccountantFailureCount,
		maxAllowedAccountantFee:        itd.MaxAllowedAccountantFee,
		bcHelper:                       itd.BlockchainHelper,
		publisher:                      itd.Publisher,
		registryAddress:                itd.Registry,
		feeProvider:                    itd.FeeProvider,
		channelAddressCalculator:       itd.ChannelAddressCalculator,
		maxRRecoveryLength:             itd.MaxRRecoveryLength,
	}
}

func calculateMaxNotReceivedExchangeMessageCount(chargeLeeway, chargePeriod time.Duration) uint64 {
	return uint64(math.Round(float64(chargeLeeway) / float64(chargePeriod)))
}

func (it *InvoiceTracker) generateInitialInvoice() error {
	agreementID, err := it.invoiceStorage.GetNewAgreementID(it.providerID)
	if err != nil {
		return errors.Wrap(err, "could not get new agreement id")
	}

	r := it.generateR()
	invoice := crypto.CreateInvoice(agreementID, it.paymentInfo.GetPrice().Amount, 0, r)
	invoice.Provider = it.providerID.Address
	it.lastInvoice = lastInvoice{
		invoice: invoice,
		r:       r,
	}
	return nil
}

// Start stars the invoice tracker
func (it *InvoiceTracker) Start() error {
	log.Debug().Msg("Starting...")
	it.timeTracker.StartTracking()

	isConsumerRegistered, err := it.bcHelper.IsRegistered(common.HexToAddress(it.registryAddress), it.peer.ToCommonAddress())
	if err != nil {
		return errors.Wrap(err, "could not check customer identity registration status")
	}

	if !isConsumerRegistered {
		return ErrConsumerNotRegistered
	}

	fees, err := it.feeProvider.FetchSettleFees()
	if err != nil {
		return errors.Wrap(err, "could not fetch fees")
	}
	it.transactorFee = fees.Fee

	fee, err := it.bcHelper.GetAccountantFee(common.HexToAddress(it.accountantID.Address))
	if err != nil {
		return errors.Wrap(err, "could not get accountants fee")
	}

	if fee > it.maxAllowedAccountantFee {
		log.Error().Msgf("Accountant fee too large, asking for %v where %v is the limit", fee, it.maxAllowedAccountantFee)
		return ErrAccountantFeeTooLarge
	}

	err = it.generateInitialInvoice()
	if err != nil {
		return errors.Wrap(err, "could not generate initial invoice")
	}

	// give the consumer a second to start up his payments before sending the first request
	firstSend := time.After(time.Second)
	for {
		select {
		case <-firstSend:
			err := it.sendInvoiceExpectExchangeMessage()
			if err != nil {
				return err
			}
		case <-it.stop:
			return nil
		case <-time.After(it.chargePeriod):
			err := it.sendInvoiceExpectExchangeMessage()
			if err != nil {
				return err
			}
		}
	}
}

func (it *InvoiceTracker) markExchangeMessageNotReceived() {
	atomic.AddUint64(&it.notReceivedExchangeMessageCount, 1)
}

func (it *InvoiceTracker) resetNotReceivedExchangeMessageCount() {
	atomic.SwapUint64(&it.notReceivedExchangeMessageCount, 0)
}

func (it *InvoiceTracker) getNotReceivedExchangeMessageCount() uint64 {
	return atomic.LoadUint64(&it.notReceivedExchangeMessageCount)
}

func (it *InvoiceTracker) generateR() []byte {
	r := make([]byte, 32)
	rand.Read(r)
	return r
}

func (it *InvoiceTracker) sendInvoiceExpectExchangeMessage() error {
	// TODO: this should be calculated according to the passed in payment period
	shouldBe := uint64(math.Trunc(it.timeTracker.Elapsed().Minutes() * float64(it.paymentInfo.GetPrice().Amount)))

	// In case we're sending a first invoice, there might be a big missmatch percentage wise on the consumer side.
	// This is due to the fact that both payment providers start at different times.
	// To compensate for this, be a bit more lenient on the first invoice - ask for a reduced amount.
	// Over the long run, this becomes redundant as the difference should become miniscule.
	if it.lastExchangeMessage.AgreementTotal == 0 {
		shouldBe = uint64(math.Trunc(float64(shouldBe) * 0.8))
		log.Debug().Msgf("Being lenient for the first payment, asking for %v", shouldBe)
	}

	r := it.generateR()
	invoice := crypto.CreateInvoice(it.lastInvoice.invoice.AgreementID, shouldBe, it.transactorFee, r)
	invoice.Provider = it.providerID.Address
	err := it.peerInvoiceSender.Send(invoice)
	if err != nil {
		return err
	}

	it.lastInvoice = lastInvoice{
		invoice: invoice,
		r:       r,
	}

	err = it.invoiceStorage.Store(it.providerID, it.peer, invoice)
	if err != nil {
		return errors.Wrap(err, "could not store invoice")
	}

	err = it.receiveExchangeMessageOrTimeout()
	if err != nil {
		handlerErr := it.handleExchangeMessageReceiveError(err)
		if handlerErr != nil {
			return err
		}
	} else {
		it.resetNotReceivedExchangeMessageCount()
	}
	return nil
}

func (it *InvoiceTracker) handleExchangeMessageReceiveError(err error) error {
	// if it's a timeout, we'll want to ignore it if we're not exceeding maxNotReceivedexchangeMessages
	if err == ErrExchangeWaitTimeout {
		it.markExchangeMessageNotReceived()
		if it.getNotReceivedExchangeMessageCount() >= it.maxNotReceivedExchangeMessages {
			return err
		}
		log.Warn().Err(err).Msg("Failed to receive exchangeMessage")
		return nil
	}
	return err
}

func (it *InvoiceTracker) incrementAccountantFailureCount() {
	atomic.AddUint64(&it.accountantFailureCount, 1)
}

func (it *InvoiceTracker) resetAccountantFailureCount() {
	atomic.SwapUint64(&it.accountantFailureCount, 0)
}

func (it *InvoiceTracker) getAccountantFailureCount() uint64 {
	return atomic.LoadUint64(&it.accountantFailureCount)
}

func (it *InvoiceTracker) validateExchangeMessage(em crypto.ExchangeMessage) error {
	peerAddr := common.HexToAddress(it.peer.Address)
	if res := em.IsMessageValid(peerAddr); !res {
		return ErrExchangeValidationFailed
	}

	signer, err := em.Promise.RecoverSigner()
	if err != nil {
		return errors.Wrap(err, "could not recover promise signature")
	}

	if signer.Hex() != peerAddr.Hex() {
		return errors.New("identity missmatch")
	}

	if em.Promise.Amount < it.lastExchangeMessage.Promise.Amount {
		log.Warn().Msgf("Consumer sent an invalid amount. Expected < %v, got %v", it.lastExchangeMessage.Promise.Amount, em.Promise.Amount)
		return errors.Wrap(ErrConsumerPromiseValidationFailed, "invalid amount")
	}

	hashlock, err := hex.DecodeString(strings.TrimPrefix(it.lastInvoice.invoice.Hashlock, "0x"))
	if err != nil {
		return errors.Wrap(err, "could not decode hashlock")
	}

	if !bytes.Equal(hashlock, em.Promise.Hashlock) {
		log.Warn().Msgf("Consumer sent an invalid hashlock. Expected %q, got %q", it.lastInvoice.invoice.Hashlock, hex.EncodeToString(em.Promise.Hashlock))
		return errors.Wrap(ErrConsumerPromiseValidationFailed, "missmatching hashlock")
	}

	addr, err := it.channelAddressCalculator.GetChannelAddress(it.peer)
	if err != nil {
		return errors.Wrap(err, "could not generate channel address")
	}

	expectedChannel, err := hex.DecodeString(strings.TrimPrefix(addr.Hex(), "0x"))
	if err != nil {
		return errors.Wrap(err, "could not decode expected chanel")
	}

	if !bytes.Equal(expectedChannel, em.Promise.ChannelID) {
		log.Warn().Msgf("Consumer sent an invalid channel address. Expected %q, got %q", addr, hex.EncodeToString(em.Promise.ChannelID))
		return errors.Wrap(ErrConsumerPromiseValidationFailed, "invalid channel address")
	}
	return nil
}

func (it *InvoiceTracker) receiveExchangeMessageOrTimeout() error {
	select {
	case pm := <-it.exchangeMessageChan:
		err := it.validateExchangeMessage(pm)
		if err != nil {
			return err
		}

		it.lastExchangeMessage = pm

		needsRevealing := false
		accountantPromise, err := it.accountantPromiseStorage.Get(it.providerID, it.accountantID)
		switch err {
		case nil:
			needsRevealing = !accountantPromise.Revealed
			break
		case ErrNotFound:
			needsRevealing = false
			break
		default:
			return errors.Wrap(err, "could not get accountant promise")
		}

		if needsRevealing {
			err = it.accountantCaller.RevealR(accountantPromise.R, it.providerID.Address, accountantPromise.AgreementID)
			if err != nil {
				log.Error().Err(err).Msg("Could not reveal R")
				it.incrementAccountantFailureCount()
				if it.getAccountantFailureCount() > it.maxAccountantFailureCount {
					return errors.Wrap(err, "could not call accountant")
				}
				log.Warn().Msg("Ignoring accountant error, we haven't reached the error threshold yet")
				return nil
			}
			it.resetAccountantFailureCount()
			accountantPromise.Revealed = true
			err = it.accountantPromiseStorage.Store(it.providerID, it.accountantID, accountantPromise)
			if err != nil {
				return errors.Wrap(err, "could not store accountant promise")
			}
			log.Debug().Msg("Accountant promise stored")
		}

		err = it.invoiceStorage.StoreR(it.providerID, it.lastInvoice.invoice.AgreementID, hex.EncodeToString(it.lastInvoice.r))
		if err != nil {
			return errors.Wrap(err, "could not store r")
		}

		promise, err := it.accountantCaller.RequestPromise(pm)
		if err != nil {
			log.Warn().Err(err).Msg("Could not call accountant")

			// TODO: handle this better
			if strings.Contains(err.Error(), "400 Bad Request") {
				recoveryError := it.initiateRRecovery()
				if recoveryError != nil {
					return errors.Wrap(err, "could not recover R")
				}
			}

			it.incrementAccountantFailureCount()
			if it.getAccountantFailureCount() > it.maxAccountantFailureCount {
				return errors.Wrap(err, "could not call accountant")
			}
			log.Warn().Msg("Ignoring accountant error, we haven't reached the error threshold yet")
			return nil
		}
		it.resetAccountantFailureCount()

		ap := AccountantPromise{
			Promise:     promise,
			R:           hex.EncodeToString(it.lastInvoice.r),
			Revealed:    false,
			AgreementID: it.lastInvoice.invoice.AgreementID,
		}
		err = it.accountantPromiseStorage.Store(it.providerID, it.accountantID, ap)
		if err != nil {
			return errors.Wrap(err, "could not store accountant promise")
		}
		log.Debug().Msg("Accountant promise stored")

		promise.R = it.lastInvoice.r
		it.publisher.Publish(AccountantPromiseTopic, AccountantPromiseEventPayload{
			Promise:      promise,
			AccountantID: it.accountantID,
			ProviderID:   it.providerID,
		})
		it.resetAccountantFailureCount()
	case <-time.After(it.exchangeMessageWaitTimeout):
		return ErrExchangeWaitTimeout
	case <-it.stop:
		return nil
	}
	return nil
}

func (it *InvoiceTracker) initiateRRecovery() error {
	currentAgreement := it.lastInvoice.invoice.AgreementID

	var minBound uint64 = 1
	if currentAgreement > it.maxRRecoveryLength {
		minBound = currentAgreement - it.maxRRecoveryLength
	}

	for i := currentAgreement; i >= minBound; i-- {
		r, err := it.invoiceStorage.GetR(it.providerID, i)
		if err != nil {
			return errors.Wrap(err, "could not get R")
		}
		err = it.accountantCaller.RevealR(r, it.providerID.Address, it.lastInvoice.invoice.AgreementID)
		if err != nil {
			log.Warn().Err(err).Msgf("revealing %v", it.lastInvoice.invoice.AgreementID)
		} else {
			log.Info().Msg("r recovered")
			return nil
		}
	}

	return errors.New("R recovery failed")
}

// Stop stops the invoice tracker.
func (it *InvoiceTracker) Stop() {
	it.once.Do(func() {
		log.Debug().Msg("Stopping...")
		close(it.stop)
	})
}
