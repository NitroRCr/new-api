package model

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// Paid top-ups by an invited user credit the inviter's pending referral
// rewards with the configured share of the credited quota, exactly once per
// order, on every supported database.
func TestAffiliateCommissionDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "aff_commission_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)

			previousDB, previousLogDB := DB, LOG_DB
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			previousRate, previousQuotaPerUnit := common.AffiliateCommissionRate, common.QuotaPerUnit
			paymentSetting := operation_setting.GetPaymentSetting()
			previousConfirmed, previousTerms := paymentSetting.ComplianceConfirmed, paymentSetting.ComplianceTermsVersion
			DB, LOG_DB = db, db
			common.SetDatabaseTypes(common.DatabaseType(dialect), common.DatabaseType(dialect))
			initCol()
			t.Cleanup(func() {
				DB, LOG_DB = previousDB, previousLogDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
				initCol()
				common.AffiliateCommissionRate, common.QuotaPerUnit = previousRate, previousQuotaPerUnit
				paymentSetting.ComplianceConfirmed, paymentSetting.ComplianceTermsVersion = previousConfirmed, previousTerms
				assert.NoError(t, sqlDB.Close())
			})

			tables := []any{&User{}, &TopUp{}, &Redemption{}, &Log{}}
			require.NoError(t, db.Migrator().DropTable(tables...))
			require.NoError(t, db.AutoMigrate(tables...))
			t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(tables...)) })
			versionSQL := "SELECT version()"
			if dialect == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("database version: %s", version)

			common.QuotaPerUnit = 7
			nextId := 0
			createUser := func(inviterId int, affQuota int) User {
				nextId++
				user := User{Id: nextId, Username: fmt.Sprintf("aff-user-%d", nextId), AffCode: fmt.Sprintf("aff-%d", nextId),
					Status: common.UserStatusEnabled, InviterId: inviterId, AffQuota: affQuota, AffHistoryQuota: affQuota}
				require.NoError(t, db.Create(&user).Error)
				return user
			}
			createOrder := func(userId int, provider string) string {
				tradeNo := fmt.Sprintf("AFF-%s-%d", strings.ToUpper(provider), nextId)
				require.NoError(t, db.Create(&TopUp{UserId: userId, Amount: 3, Money: 3, TradeNo: tradeNo, PaymentMethod: provider,
					PaymentProvider: provider, Status: common.TopUpStatusPending, CreateTime: common.GetTimestamp()}).Error)
				return tradeNo
			}
			affiliate := func(userId int) User {
				var user User
				require.NoError(t, db.Unscoped().Select("quota", "aff_quota", "aff_history").Where("id = ?", userId).First(&user).Error)
				return user
			}
			commissionLogs := func(userId int) int64 {
				var count int64
				require.NoError(t, db.Model(&Log{}).Where("user_id = ? AND type = ?", userId, LogTypeSystem).Count(&count).Error)
				return count
			}

			tests := []struct {
				name           string
				rate           float64
				compliance     bool
				noInviter      bool
				inviterDeleted bool
				inviterFull    bool
				complete       func(t *testing.T, userId int)
				wantCommission int
			}{
				{
					// 3 units * 7 = 21 quota; 10% of 21 rounds down to 2.
					name: "epay pays commission once despite repeated callbacks", rate: 10, compliance: true,
					complete: func(t *testing.T, userId int) {
						tradeNo := createOrder(userId, PaymentProviderEpay)
						_, err := RechargeEpay(tradeNo, "", "127.0.0.1")
						require.NoError(t, err)
						alreadyDone, err := RechargeEpay(tradeNo, "", "127.0.0.1")
						require.NoError(t, err)
						assert.True(t, alreadyDone)
					},
					wantCommission: 2,
				},
				{
					name: "administrator completion pays a fractional rate", rate: 50.5, compliance: true,
					complete: func(t *testing.T, userId int) {
						tradeNo := createOrder(userId, PaymentProviderEpay)
						require.NoError(t, ManualCompleteTopUp(tradeNo, "127.0.0.1"))
						require.NoError(t, ManualCompleteTopUp(tradeNo, "127.0.0.1"))
					},
					wantCommission: 10,
				},
				{
					name: "waffo pays commission", rate: 100, compliance: true,
					complete: func(t *testing.T, userId int) {
						require.NoError(t, RechargeWaffo(createOrder(userId, PaymentProviderWaffo), "127.0.0.1"))
					},
					wantCommission: 21,
				},
				{
					name: "zero rate pays nothing", rate: 0, compliance: true,
					complete: func(t *testing.T, userId int) {
						_, err := RechargeEpay(createOrder(userId, PaymentProviderEpay), "", "127.0.0.1")
						require.NoError(t, err)
					},
				},
				{
					name: "unconfirmed compliance pays nothing", rate: 10, compliance: false,
					complete: func(t *testing.T, userId int) {
						_, err := RechargeEpay(createOrder(userId, PaymentProviderEpay), "", "127.0.0.1")
						require.NoError(t, err)
					},
				},
				{
					name: "user without inviter pays nothing", rate: 10, compliance: true, noInviter: true,
					complete: func(t *testing.T, userId int) {
						_, err := RechargeEpay(createOrder(userId, PaymentProviderEpay), "", "127.0.0.1")
						require.NoError(t, err)
					},
				},
				{
					name: "deleted inviter earns nothing and the top-up succeeds", rate: 10, compliance: true, inviterDeleted: true,
					complete: func(t *testing.T, userId int) {
						_, err := RechargeEpay(createOrder(userId, PaymentProviderEpay), "", "127.0.0.1")
						require.NoError(t, err)
					},
				},
				{
					name: "inviter at the wallet ceiling earns nothing and the top-up succeeds", rate: 10, compliance: true, inviterFull: true,
					complete: func(t *testing.T, userId int) {
						_, err := RechargeEpay(createOrder(userId, PaymentProviderEpay), "", "127.0.0.1")
						require.NoError(t, err)
					},
				},
				{
					name: "redemption code pays nothing", rate: 10, compliance: true,
					complete: func(t *testing.T, userId int) {
						key := fmt.Sprintf("%032d", nextId)
						require.NoError(t, db.Create(&Redemption{Id: nextId, Name: "aff", Key: key, Quota: 21,
							Status: common.RedemptionCodeStatusEnabled, CreatedTime: common.GetTimestamp()}).Error)
						_, err := Redeem(key, userId)
						require.NoError(t, err)
					},
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					common.AffiliateCommissionRate = tt.rate
					paymentSetting.ComplianceConfirmed = tt.compliance
					paymentSetting.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion

					startingAff := 0
					if tt.inviterFull {
						startingAff = common.MaxWalletQuota
					}
					inviter := createUser(0, startingAff)
					if tt.inviterDeleted {
						require.NoError(t, db.Delete(&User{}, inviter.Id).Error)
					}
					inviterId := inviter.Id
					if tt.noInviter {
						inviterId = 0
					}
					invitee := createUser(inviterId, 0)

					tt.complete(t, invitee.Id)

					assert.Equal(t, 21, affiliate(invitee.Id).Quota)
					got := affiliate(inviter.Id)
					assert.Equal(t, startingAff+tt.wantCommission, got.AffQuota)
					assert.Equal(t, startingAff+tt.wantCommission, got.AffHistoryQuota)
					assert.Zero(t, got.Quota)
					wantLogs := int64(0)
					if tt.wantCommission > 0 {
						wantLogs = 1
					}
					assert.Equal(t, wantLogs, commissionLogs(inviter.Id))
				})
			}
		})
	}
}
