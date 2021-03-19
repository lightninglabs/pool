package sidecar

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SignOffer adds a signature over the offer digest to the given ticket.
func SignOffer(ctx context.Context, ticket *Ticket,
	signingKeyLoc keychain.KeyLocator, signer lndclient.SignerClient) error {

	// The ticket needs to be in the correct state for us to sign it.
	if ticket == nil || ticket.State < StateOffered {
		return fmt.Errorf("ticket is in invalid state")
	}

	// The ticket also needs to have a signed offer.
	offer := ticket.Offer
	if offer.SignPubKey == nil || offer.SigOfferDigest != nil {
		return fmt.Errorf("offer in ticket is not in expected state " +
			"to be signed")
	}

	// Let's sign the offer part of the ticket with our node's identity key
	// now.
	offerDigest, err := ticket.OfferDigest()
	if err != nil {
		return fmt.Errorf("error digesting offer: %v", err)
	}
	rawSig, err := signer.SignMessage(ctx, offerDigest[:], signingKeyLoc)
	if err != nil {
		return fmt.Errorf("error signing offer: %v", err)
	}
	wireSig, err := lnwire.NewSigFromRawSignature(rawSig)
	if err != nil {
		return fmt.Errorf("error parsing raw signature: %v", err)
	}
	ecSig, err := wireSig.ToSignature()
	if err != nil {
		return fmt.Errorf("error parsing EC signature: %v", err)
	}
	ticket.Offer.SigOfferDigest = ecSig

	return nil
}

// VerifyOffer verifies the state of a ticket to be in the offered state and
// also makes sure the offer signature is valid.
func VerifyOffer(ctx context.Context, ticket *Ticket,
	signer lndclient.SignerClient) error {

	// The ticket needs to be in the correct state for us to verify it.
	if ticket == nil || ticket.State < StateOffered {
		return fmt.Errorf("ticket is in invalid state")
	}

	// The ticket also needs to have a signed offer.
	offer := ticket.Offer
	if offer.SignPubKey == nil || offer.SigOfferDigest == nil {
		return fmt.Errorf("offer in ticket is not signed")
	}

	var offerPubKeyRaw [33]byte
	copy(offerPubKeyRaw[:], ticket.Offer.SignPubKey.SerializeCompressed())

	// Make sure the provider's signature over the offer is valid.
	offerDigest, err := ticket.OfferDigest()
	if err != nil {
		return fmt.Errorf("error calculating offer digest: %v", err)
	}
	sigValid, err := signer.VerifyMessage(
		ctx, offerDigest[:], offer.SigOfferDigest.Serialize(),
		offerPubKeyRaw,
	)
	if err != nil {
		return fmt.Errorf("unable to verify offer signature: %v", err)
	}
	if !sigValid {
		return fmt.Errorf("signature not valid for public key %x",
			offerPubKeyRaw[:])
	}

	return nil
}

// SignOrder adds the order part to a ticket and signs it, adding the signature
// as well.
func SignOrder(ctx context.Context, ticket *Ticket, bidNonce [32]byte,
	signingKeyLoc keychain.KeyLocator, signer lndclient.SignerClient) error {

	// The ticket needs to be in the correct state for us to sign it.
	if ticket == nil || ticket.State < StateRegistered {
		return fmt.Errorf("ticket is in invalid state")
	}

	// The ticket also needs to have a signed offer.
	offer := ticket.Offer
	if offer.SignPubKey == nil || offer.SigOfferDigest == nil {
		return fmt.Errorf("offer in ticket is not signed")
	}

	// Add the bid order's nonce to the order part of the ticket now.
	if ticket.Order == nil {
		ticket.Order = &Order{}
	}
	ticket.Order.BidNonce = bidNonce
	ticket.State = StateOrdered

	// Let's sign the order part of the ticket with our node's identity key
	// now.
	orderDigest, err := ticket.OrderDigest()
	if err != nil {
		return fmt.Errorf("error digesting order: %v", err)
	}
	rawSig, err := signer.SignMessage(ctx, orderDigest[:], signingKeyLoc)
	if err != nil {
		return fmt.Errorf("error signing order: %v", err)
	}
	wireSig, err := lnwire.NewSigFromRawSignature(rawSig)
	if err != nil {
		return fmt.Errorf("error parsing raw signature: %v", err)
	}
	ecSig, err := wireSig.ToSignature()
	if err != nil {
		return fmt.Errorf("error parsing EC signature: %v", err)
	}
	ticket.Order.SigOrderDigest = ecSig

	return nil
}

// CheckOfferParams makes sure the offer parameters of a sidecar ticket are
// valid and sane.
func CheckOfferParams(capacity, pushAmt, baseSupplyUnit btcutil.Amount) error {
	if capacity == 0 || capacity%baseSupplyUnit != 0 {
		return fmt.Errorf("channel capacity must be positive multiple "+
			"of %d", baseSupplyUnit)
	}

	if pushAmt > capacity {
		return fmt.Errorf("self channel balance must be smaller than " +
			"or equal to capacity")
	}

	return nil
}

// CheckOfferParamsForOrder makes sure that the order parameters in a
// sidecar offer are formally valid, sane and match the order parameters.
func CheckOfferParamsForOrder(offer Offer, bidAmt, bidMinUnitsMatch,
	baseSupplyUnit btcutil.Amount) error {

	err := CheckOfferParams(offer.Capacity, offer.PushAmt, baseSupplyUnit)
	if err != nil {
		return err
	}

	if offer.Capacity != bidAmt {
		return fmt.Errorf("invalid bid amount %v, must match sidecar "+
			"ticket's capacity %v", bidAmt, offer.Capacity)
	}

	if offer.Capacity != bidMinUnitsMatch*baseSupplyUnit {
		return fmt.Errorf("invalid min units match %v, must match "+
			"sidecar ticket's capacity %v",
			bidMinUnitsMatch*baseSupplyUnit, offer.Capacity)
	}

	return nil
}
