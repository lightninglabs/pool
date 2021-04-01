package sidecar

import (
	"fmt"

	"github.com/btcsuite/btcutil"
)

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
