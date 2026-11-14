package biz

import (
	"math"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/objects"
)

func normalizeChannelBalanceCapacity(snapshot *objects.ChannelKeyBalanceSnapshot, manualLimit *float64) {
	if snapshot == nil {
		return
	}

	if !finiteBalanceValue(snapshot.Used) {
		snapshot.Used = nil
	}
	if !finiteBalanceValue(snapshot.Limit) {
		snapshot.Limit = nil
	}
	if !finiteBalanceValue(snapshot.Remaining) {
		snapshot.Remaining = nil
	}
	if !finiteBalanceValue(snapshot.UsageRatio) {
		snapshot.UsageRatio = nil
	}

	if snapshot.Used == nil {
		snapshot.Used = balanceAmountValue(snapshot.Components, objects.BalanceAmountKindUsed)
	}
	if snapshot.Limit == nil {
		snapshot.Limit = balanceAmountValue(snapshot.Components, objects.BalanceAmountKindLimit)
	}
	if snapshot.Limit == nil && (snapshot.Used != nil || snapshot.Remaining != nil) {
		snapshot.Limit = balanceAmountValue(snapshot.Components, objects.BalanceAmountKindTotal)
	}
	if snapshot.Remaining == nil {
		snapshot.Remaining = balanceAmountValue(snapshot.Components, objects.BalanceAmountKindRemaining)
	}
	if snapshot.Remaining == nil {
		snapshot.Remaining = balanceAmountValue(snapshot.Components, objects.BalanceAmountKindAvailable)
	}

	if snapshot.Limit == nil && manualLimit != nil && isValidManualBalanceLimit(*manualLimit) {
		snapshot.Limit = lo.ToPtr(*manualLimit)
	}

	if snapshot.Remaining == nil && snapshot.PrimaryBalance != nil && finiteBalanceValue(&snapshot.PrimaryBalance.Amount) {
		switch snapshot.PrimaryBalance.Kind {
		case objects.BalanceAmountKindUsed, objects.BalanceAmountKindLimit:
			// These components describe capacity rather than current balance.
		default:
			snapshot.Remaining = lo.ToPtr(snapshot.PrimaryBalance.Amount)
		}
	}

	if snapshot.Limit == nil && snapshot.Used != nil && snapshot.Remaining != nil {
		snapshot.Limit = lo.ToPtr(*snapshot.Used + *snapshot.Remaining)
	}
	if snapshot.Used == nil && snapshot.Limit != nil && snapshot.Remaining != nil {
		used := *snapshot.Limit - *snapshot.Remaining
		if used < 0 {
			used = 0
		}
		snapshot.Used = lo.ToPtr(used)
	}
	if snapshot.Remaining == nil && snapshot.Limit != nil && snapshot.Used != nil {
		remaining := *snapshot.Limit - *snapshot.Used
		if remaining < 0 {
			remaining = 0
		}
		snapshot.Remaining = lo.ToPtr(remaining)
	}

	if snapshot.Limit != nil && *snapshot.Limit > 0 && snapshot.UsageRatio == nil && snapshot.Used != nil {
		ratio := *snapshot.Used / *snapshot.Limit
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		snapshot.UsageRatio = lo.ToPtr(ratio)
	}
	if snapshot.UsageRatio != nil {
		ratio := *snapshot.UsageRatio
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		snapshot.UsageRatio = lo.ToPtr(ratio)
	}

	if snapshot.Limit != nil && *snapshot.Limit > 0 && snapshot.Remaining != nil {
		available := *snapshot.Remaining > 0
		if snapshot.Available == nil || !available {
			snapshot.Available = &available
		}
	}
}

func balanceAmountValue(amounts []objects.BalanceAmount, kind objects.BalanceAmountKind) *float64 {
	for _, amount := range amounts {
		if amount.Kind == kind && finiteBalanceValue(&amount.Amount) {
			return lo.ToPtr(amount.Amount)
		}
	}
	return nil
}

func finiteBalanceValue(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func isValidManualBalanceLimit(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}
