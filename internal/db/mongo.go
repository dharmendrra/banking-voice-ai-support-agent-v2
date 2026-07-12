package db

import (
	"banking-voice-ai-agent/internal/telemetry"

	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.opentelemetry.io/otel/attribute"
)

type MongoManager struct {
	Client *mongo.Client
	DB     *mongo.Database
}

type User struct {
	UserID string `bson:"user_id"`
	Name   string `bson:"name"`
	Email  string `bson:"email"`
}

type Account struct {
	UserID    string  `bson:"user_id"`
	AccountNo string  `bson:"account_no"`
	Balance   float64 `bson:"balance"`
	Currency  string  `bson:"currency"`
}

type Transaction struct {
	UserID      string    `bson:"user_id"`
	AccountNo   string    `bson:"account_no"`
	Amount      float64   `bson:"amount"`
	Description string    `bson:"description"`
	Date        time.Time `bson:"date"`
}

type Card struct {
	UserID   string `bson:"user_id"`
	CardNo   string `bson:"card_no"`
	CardType string `bson:"card_type"` // e.g. "credit", "debit"
	Status   string `bson:"status"`    // e.g. "active", "blocked"
	DueDate  string `bson:"due_date"`
}

type TransferRecord struct {
	UniqueRefNo  string    `bson:"unique_ref_no"`
	UserID       string    `bson:"user_id"`
	ToAccount    string    `bson:"to_account"`
	Amount       float64   `bson:"amount"`
	PaymentRefNo string    `bson:"payment_ref_no"`
	Status       string    `bson:"status"`
	CreatedAt    time.Time `bson:"created_at"`
}

func NewMongoManager(uri string) (*MongoManager, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mongo: %w", err)
	}

	// Ping the DB
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping mongo: %w", err)
	}

	db := client.Database("banking")
	mgr := &MongoManager{
		Client: client,
		DB:     db,
	}

	if err := mgr.initAndSeed(ctx); err != nil {
		return nil, fmt.Errorf("failed to seed mongo: %w", err)
	}

	return mgr, nil
}

func (m *MongoManager) initAndSeed(ctx context.Context) error {
	// Create unique index on transfers.unique_ref_no
	transfersColl := m.DB.Collection("transfers")
	_, err := transfersColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "unique_ref_no", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		log.Printf("Warning: failed to create unique index on unique_ref_no: %v", err)
	}

	// Check if seeded
	usersColl := m.DB.Collection("users")
	count, err := usersColl.CountDocuments(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("failed to count users: %w", err)
	}

	if count > 0 {
		log.Println("MongoDB already seeded.")
		return nil
	}

	log.Println("Seeding MongoDB with mock banking data...")

	// Seed User
	mockUser := User{
		UserID: "mock_user_123",
		Name:   "Dharmendra Yadav",
		Email:  "dharmendra@example.com",
	}
	_, err = usersColl.InsertOne(ctx, mockUser)
	if err != nil {
		return fmt.Errorf("failed to seed user: %w", err)
	}

	// Seed Account
	accountsColl := m.DB.Collection("accounts")
	mockAccount := Account{
		UserID:    "mock_user_123",
		AccountNo: "1234567890",
		Balance:   4567.89,
		Currency:  "INR",
	}
	_, err = accountsColl.InsertOne(ctx, mockAccount)
	if err != nil {
		return fmt.Errorf("failed to seed account: %w", err)
	}

	// Seed Cards
	cardsColl := m.DB.Collection("cards")
	mockCards := []interface{}{
		Card{
			UserID:   "mock_user_123",
			CardNo:   "4321-8765-9012-3456",
			CardType: "credit",
			Status:   "active",
			DueDate:  "2026-07-25",
		},
		Card{
			UserID:   "mock_user_123",
			CardNo:   "5678-1234-5678-4321",
			CardType: "debit",
			Status:   "active",
			DueDate:  "2026-07-25",
		},
	}
	_, err = cardsColl.InsertMany(ctx, mockCards)
	if err != nil {
		return fmt.Errorf("failed to seed card: %w", err)
	}

	// Seed Transactions
	txsColl := m.DB.Collection("transactions")
	mockTxs := []interface{}{
		Transaction{
			UserID:      "mock_user_123",
			AccountNo:   "1234567890",
			Amount:      -150.00,
			Description: "Grocery Store",
			Date:        time.Now().Add(-24 * time.Hour),
		},
		Transaction{
			UserID:      "mock_user_123",
			AccountNo:   "1234567890",
			Amount:      2500.00,
			Description: "Salary Credit",
			Date:        time.Now().Add(-10 * 24 * time.Hour),
		},
		Transaction{
			UserID:      "mock_user_123",
			AccountNo:   "1234567890",
			Amount:      -450.00,
			Description: "Electricity Bill",
			Date:        time.Now().Add(-5 * 24 * time.Hour),
		},
	}
	_, err = txsColl.InsertMany(ctx, mockTxs)
	if err != nil {
		return fmt.Errorf("failed to seed transactions: %w", err)
	}

	log.Println("MongoDB seeding completed successfully.")
	return nil
}

// GetBalance gets account balance for user
func (m *MongoManager) GetBalance(ctx context.Context, userID string) (float64, string, error) {
	ctx, span := telemetry.Step(ctx, "bank.get_balance",
		attribute.String("db.system", "mongodb"),
		attribute.String("db.collection", "accounts"),
		attribute.String("db.operation", "find_one"),
	)
	defer span.End()
	var acc Account
	err := m.DB.Collection("accounts").FindOne(ctx, bson.M{"user_id": userID}).Decode(&acc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, "", fmt.Errorf("no account found for user %s", userID)
		}
		return 0, "", err
	}
	return acc.Balance, acc.Currency, nil
}

// GetTransactions gets recent N transactions for user
func (m *MongoManager) GetTransactions(ctx context.Context, userID string, limit int64) ([]Transaction, error) {
	ctx, span := telemetry.Step(ctx, "bank.get_transactions",
		attribute.String("db.system", "mongodb"),
		attribute.String("db.collection", "transactions"),
		attribute.String("db.operation", "find_many"),
		attribute.Int64("db.limit", limit),
	)
	defer span.End()
	opts := options.Find().SetSort(bson.D{{Key: "date", Value: -1}}).SetLimit(limit)
	cursor, err := m.DB.Collection("transactions").Find(ctx, bson.M{"user_id": userID}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var txs []Transaction
	if err := cursor.All(ctx, &txs); err != nil {
		return nil, err
	}
	return txs, nil
}

// GetDueDate gets card due date
func (m *MongoManager) GetDueDate(ctx context.Context, userID string, cardType string) (string, error) {
	ctx, span := telemetry.Step(ctx, "bank.get_due_date",
		attribute.String("db.system", "mongodb"),
		attribute.String("db.collection", "cards"),
		attribute.String("db.operation", "find_one"),
		attribute.String("db.card_type", cardType),
	)
	defer span.End()
	var card Card
	err := m.DB.Collection("cards").FindOne(ctx, bson.M{"user_id": userID, "card_type": cardType}).Decode(&card)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", fmt.Errorf("no credit card found for user %s", userID)
		}
		return "", err
	}
	return card.DueDate, nil
}

// BlockCard sets credit card status to blocked
func (m *MongoManager) BlockCard(ctx context.Context, userID string, cardType string) (bool, error) {
	ctx, span := telemetry.Step(ctx, "bank.block_card",
		attribute.String("db.system", "mongodb"),
		attribute.String("db.collection", "cards"),
		attribute.String("db.operation", "update_one"),
		attribute.String("db.card_type", cardType),
	)
	defer span.End()
	res, err := m.DB.Collection("cards").UpdateOne(
		ctx,
		bson.M{"user_id": userID, "card_type": cardType},
		bson.M{"$set": bson.M{"status": "blocked"}},
	)
	if err != nil {
		return false, err
	}
	return res.ModifiedCount > 0 || res.MatchedCount > 0, nil
}

// Transfer transfers money from the account and records the transaction idempotently
func (m *MongoManager) Transfer(ctx context.Context, userID string, toAccount string, amount float64, uniqueRefNo string) (string, error) {
	ctx, span := telemetry.Step(ctx, "bank.transfer",
		attribute.String("db.system", "mongodb"),
		attribute.String("db.collection", "transfers"),
		attribute.String("db.operation", "transaction_transfer"),
		attribute.Float64("db.amount", amount),
	)
	defer span.End()
	if uniqueRefNo == "" {
		return "", fmt.Errorf("unique_ref_no is required")
	}

	transfersColl := m.DB.Collection("transfers")

	// Check if already executed
	var existing TransferRecord
	err := transfersColl.FindOne(ctx, bson.M{"unique_ref_no": uniqueRefNo}).Decode(&existing)
	if err == nil {
		log.Printf("[Idempotency] Duplicate transfer detected, returning original payment_ref_no %s", existing.PaymentRefNo)
		return existing.PaymentRefNo, nil
	}

	if !errors.Is(err, mongo.ErrNoDocuments) {
		return "", fmt.Errorf("failed checking idempotency: %w", err)
	}

	// Execute transfer
	accountsColl := m.DB.Collection("accounts")

	// Find source account
	var srcAcc Account
	err = accountsColl.FindOne(ctx, bson.M{"user_id": userID}).Decode(&srcAcc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch user account: %w", err)
	}

	if srcAcc.Balance < amount {
		return "", fmt.Errorf("insufficient balance: account has %.2f, trying to transfer %.2f", srcAcc.Balance, amount)
	}

	// Update balance
	_, err = accountsColl.UpdateOne(
		ctx,
		bson.M{"user_id": userID},
		bson.M{"$inc": bson.M{"balance": -amount}},
	)
	if err != nil {
		return "", fmt.Errorf("failed to deduct balance: %w", err)
	}

	// Generate transaction payment reference ID
	paymentRefNo := fmt.Sprintf("PAY-REF-%d", time.Now().UnixNano()/1000)

	// Insert transaction history
	txsColl := m.DB.Collection("transactions")
	_, err = txsColl.InsertOne(ctx, Transaction{
		UserID:      userID,
		AccountNo:   srcAcc.AccountNo,
		Amount:      -amount,
		Description: fmt.Sprintf("Transfer to %s", toAccount),
		Date:        time.Now(),
	})
	if err != nil {
		log.Printf("Warning: failed to insert transaction record: %v", err)
	}

	// Insert into transfers idempotency collection
	record := TransferRecord{
		UniqueRefNo:  uniqueRefNo,
		UserID:       userID,
		ToAccount:    toAccount,
		Amount:       amount,
		PaymentRefNo: paymentRefNo,
		Status:       "completed",
		CreatedAt:    time.Now(),
	}

	_, err = transfersColl.InsertOne(ctx, record)
	if err != nil {
		// Unique key index violation could happen if concurrent request raced
		if mongo.IsDuplicateKeyError(err) {
			// Deduct balance rollback or refund could be done here, but since we are single instance/simple, we just try to fetch the winner
			var winner TransferRecord
			if err2 := transfersColl.FindOne(ctx, bson.M{"unique_ref_no": uniqueRefNo}).Decode(&winner); err2 == nil {
				return winner.PaymentRefNo, nil
			}
		}
		return "", fmt.Errorf("failed to record transfer idempotency: %w", err)
	}

	return paymentRefNo, nil
}
