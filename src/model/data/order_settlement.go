package data

import "gorm.io/gorm"

// applySettlementRecordScope keeps one database row per completed payment.
// A parent paid through a sub-order remains successful for callback handling,
// but pay_by_sub_id marks it as a non-settlement row for reporting purposes.
func applySettlementRecordScope(tx *gorm.DB) *gorm.DB {
	return tx.Where("COALESCE(pay_by_sub_id, 0) = 0")
}
