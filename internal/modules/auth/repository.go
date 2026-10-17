package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type UserRepository interface {
	Create(ctx context.Context, rec userRecord) (User, error)
	FindByEmail(ctx context.Context, email string) (userRecord, bool, error)
	FindByID(ctx context.Context, id primitive.ObjectID) (User, bool, error)
	UpdateMe(ctx context.Context, id primitive.ObjectID, name *string, phone *string, settings *UserSettings) (User, error)
	ListAll(ctx context.Context) ([]User, error)
	EnsureIndexes(ctx context.Context) error
	// TouchLogin stamps the start of a new session (loginAt) and refreshes
	// lastSeenAt to the same instant. Called once per successful sign-in.
	TouchLogin(ctx context.Context, id primitive.ObjectID, at time.Time) error
	// TouchLastSeen refreshes lastSeenAt on an authenticated request. If the
	// user's previous activity predates idleCutoff they are considered to
	// have started a fresh session, so loginAt is moved to at as well.
	// Without this, loginAt would stay pinned to the original sign-in and
	// "session length" would grow for as long as the JWT stays valid.
	TouchLastSeen(ctx context.Context, id primitive.ObjectID, at time.Time, idleCutoff time.Time) error
}

type MongoUserRepository struct {
	col *mongo.Collection
}

func NewMongoUserRepository(db *mongo.Database) *MongoUserRepository {
	return &MongoUserRepository{col: db.Collection("users")}
}

func (r *MongoUserRepository) EnsureIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "email", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys:    bson.D{{Key: "phone", Value: 1}},
			Options: options.Index().SetUnique(true).SetSparse(true),
		},
		// Range-scanned by the admin analytics overview (signup growth and
		// daily-active-users). Without these both counts are collection scans.
		{Keys: bson.D{{Key: "createdAt", Value: -1}}},
		{Keys: bson.D{{Key: "lastSeenAt", Value: -1}}},
	}

	_, err := r.col.Indexes().CreateMany(ctx, indexes)
	return err
}

func (r *MongoUserRepository) Create(ctx context.Context, rec userRecord) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := r.col.InsertOne(ctx, rec)
	if err != nil {
		if isDuplicateKey(err) {
			if strings.Contains(strings.ToLower(err.Error()), "email") {
				return User{}, errEmailExists
			}
			if strings.Contains(strings.ToLower(err.Error()), "phone") {
				return User{}, errPhoneExists
			}
			return User{}, errEmailExists
		}
		return User{}, err
	}
	return rec.User, nil
}

func (r *MongoUserRepository) FindByEmail(ctx context.Context, email string) (userRecord, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var rec userRecord
	err := r.col.FindOne(ctx, bson.M{"email": strings.ToLower(strings.TrimSpace(email))}).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return userRecord{}, false, nil
		}
		return userRecord{}, false, err
	}
	return rec, true, nil
}

func (r *MongoUserRepository) FindByID(ctx context.Context, id primitive.ObjectID) (User, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var rec userRecord
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	return rec.User, true, nil
}

func (r *MongoUserRepository) ListAll(ctx context.Context) ([]User, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cur, err := r.col.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []User
	for cur.Next(ctx) {
		var rec userRecord
		if err := cur.Decode(&rec); err != nil {
			return nil, err
		}
		out = append(out, rec.User)
	}
	return out, cur.Err()
}

func (r *MongoUserRepository) UpdateMe(ctx context.Context, id primitive.ObjectID, name *string, phone *string, settings *UserSettings) (User, error) {
	set := bson.M{}
	if name != nil {
		set["name"] = strings.TrimSpace(*name)
	}
	if phone != nil {
		set["phone"] = strings.TrimSpace(*phone)
	}
	if settings != nil {
		set["settings"] = *settings
	}
	if len(set) == 0 {
		return User{}, errNothingToUpdate
	}
	set["updatedAt"] = time.Now().UTC()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res := r.col.FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": set}, options.FindOneAndUpdate().SetReturnDocument(options.After))
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return User{}, errUserNotFound
		}
		if isDuplicateKey(err) {
			return User{}, errPhoneExists
		}
		return User{}, err
	}

	var rec userRecord
	if err := res.Decode(&rec); err != nil {
		return User{}, err
	}
	return rec.User, nil
}

func (r *MongoUserRepository) TouchLogin(ctx context.Context, id primitive.ObjectID, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := r.col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"loginAt": at, "lastSeenAt": at}})
	return err
}

func (r *MongoUserRepository) TouchLastSeen(ctx context.Context, id primitive.ObjectID, at time.Time, idleCutoff time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Pipeline update so the idle check reads the stored lastSeenAt and the
	// whole decision stays atomic. Inside one $set every expression sees the
	// pre-update document, so "$lastSeenAt" is the previous value.
	//
	// A missing lastSeenAt (account that predates this field) compares as
	// null, which sorts below any date, so it takes the new-session branch.
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.M{
			"loginAt": bson.M{"$cond": bson.A{
				bson.M{"$lt": bson.A{"$lastSeenAt", idleCutoff}},
				at,
				bson.M{"$ifNull": bson.A{"$loginAt", at}},
			}},
			"lastSeenAt": at,
		}}},
	}

	_, err := r.col.UpdateOne(ctx, bson.M{"_id": id}, pipeline)
	return err
}

func isDuplicateKey(err error) bool {
	var we mongo.WriteException
	if errors.As(err, &we) {
		for _, e := range we.WriteErrors {
			if e.Code == 11000 {
				return true
			}
		}
	}

	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) {
		for _, e := range bwe.WriteErrors {
			if e.Code == 11000 {
				return true
			}
		}
	}
	return false
}

type InMemoryUserRepository struct {
	users     map[string]userRecord
	usersByID map[primitive.ObjectID]userRecord
}

func NewInMemoryUserRepository() *InMemoryUserRepository {
	return &InMemoryUserRepository{
		users:     make(map[string]userRecord),
		usersByID: make(map[primitive.ObjectID]userRecord),
	}
}

func (r *InMemoryUserRepository) EnsureIndexes(ctx context.Context) error {
	return nil
}

func (r *InMemoryUserRepository) Create(ctx context.Context, rec userRecord) (User, error) {
	emailKey := strings.ToLower(strings.TrimSpace(rec.Email))
	if _, exists := r.users[emailKey]; exists {
		return User{}, errEmailExists
	}
	if rec.ID.IsZero() {
		rec.ID = primitive.NewObjectID()
	}
	r.users[emailKey] = rec
	r.usersByID[rec.ID] = rec
	return rec.User, nil
}

func (r *InMemoryUserRepository) FindByEmail(ctx context.Context, email string) (userRecord, bool, error) {
	emailKey := strings.ToLower(strings.TrimSpace(email))
	rec, exists := r.users[emailKey]
	if !exists {
		return userRecord{}, false, nil
	}
	return rec, true, nil
}

func (r *InMemoryUserRepository) FindByID(ctx context.Context, id primitive.ObjectID) (User, bool, error) {
	rec, exists := r.usersByID[id]
	if !exists {
		return User{}, false, nil
	}
	return rec.User, true, nil
}

func (r *InMemoryUserRepository) ListAll(ctx context.Context) ([]User, error) {
	out := make([]User, 0, len(r.usersByID))
	for _, rec := range r.usersByID {
		out = append(out, rec.User)
	}
	return out, nil
}

func (r *InMemoryUserRepository) UpdateMe(ctx context.Context, id primitive.ObjectID, name *string, phone *string, settings *UserSettings) (User, error) {
	rec, exists := r.usersByID[id]
	if !exists {
		return User{}, errUserNotFound
	}
	if name != nil {
		rec.Name = strings.TrimSpace(*name)
	}
	if phone != nil {
		rec.Phone = strings.TrimSpace(*phone)
	}
	if settings != nil {
		rec.Settings = *settings
	}
	rec.UpdatedAt = time.Now().UTC()

	r.users[strings.ToLower(rec.Email)] = rec
	r.usersByID[id] = rec
	return rec.User, nil
}

func (r *InMemoryUserRepository) TouchLogin(ctx context.Context, id primitive.ObjectID, at time.Time) error {
	rec, exists := r.usersByID[id]
	if !exists {
		return errUserNotFound
	}
	rec.LoginAt = at
	rec.LastSeenAt = at
	r.users[strings.ToLower(rec.Email)] = rec
	r.usersByID[id] = rec
	return nil
}

func (r *InMemoryUserRepository) TouchLastSeen(ctx context.Context, id primitive.ObjectID, at time.Time, idleCutoff time.Time) error {
	rec, exists := r.usersByID[id]
	if !exists {
		return errUserNotFound
	}
	// Mirrors the Mongo pipeline: idle for long enough (or never seen before)
	// starts a new session.
	if rec.LastSeenAt.Before(idleCutoff) || rec.LoginAt.IsZero() {
		rec.LoginAt = at
	}
	rec.LastSeenAt = at
	r.users[strings.ToLower(rec.Email)] = rec
	r.usersByID[id] = rec
	return nil
}

