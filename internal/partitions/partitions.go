package partitions

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	//
	// In order for $sample to use a pseudo-random cursor (good) instead of doing a collection scan (bad), the
	// number of documents for $sample to fetch must be <5% of the total number of documents in the collection.
	// See: https://docs.mongodb.com/manual/reference/operator/aggregation/sample/#behavior
	//
	// We'd like to sample closer to 5% since, in theory, that gives us a better spread of documents to use
	// as partition boundaries. We choose to sample 4% of the collection for safety, in case the number of
	// documents increases between us getting the document count and us doing the actual sampling. It's still
	// possible that $sample does a collection scan if the number of documents increases very quickly, but
	// that should be very rare.
	//
	defaultSampleRate = 0.04

	//
	// The minimum number of documents $sample requires in order to use a pseudo-random cursor.
	// See: https://docs.mongodb.com/manual/reference/operator/aggregation/sample/#behavior
	//
	defaultSampleMinNumDocs = 101

	//
	// The maximum number of documents to sample per partition. This is intended as an upper limit
	// on sampling 4% of a collection for performance reasons. See the trial results in REP-332.
	//
	defaultMaxNumDocsToSamplePerPartition = 10

	//
	// In general, this constant should be set to (16 MB) / (defaultSampleRate) = (16 MB) / (4%) = 400 MB.
	// This is the smallest guaranteeable average partition size for the scenario where each document is
	// the maximum allowed size of 16 MB. Proof:
	//
	//   - Assume each document is the maximum allowed size of 16 MB.
	//   - We always sample 4% of the documents in a collection, or 1 out of every 25 documents.
	//   - Minimizing the partition sizes means maximizing the number of partitions, i.e. using
	//     every sampled document as a partition bound.
	//   - So each partition contains 25 documents, on average.
	//   - Average partition size = (average doc size) * (average number of docs per partition)
	//                            = (16 MB / doc)      * (25 docs / partition)
	//                            = (400 MB / partition)
	//
	// If this constant is set to less than 400 MB for a 4% sample rate, then that smaller partition size
	// cannot be guaranteed if docs are very large, and the $bucketAuto stage will return partitions that
	// average 400 MB anyway in such cases. In other cases where doc size limits are not reached, a
	// partition size under 400 MB would be honored.
	//
	// So 400 MB happens to be the lower bound on the partition sizes for a 4% sample rate, and also a
	// sensible partition size that's not too small or too large. This gives a reasonable expectation
	// for how large partitions will be, regardless of average document size.
	//
	defaultPartitionSizeInBytes = 400 * 1024 * 1024 // = 400 MB
)

// Partitions is a slice of partitions.
type Partitions struct {
	logger     *logger.Logger
	partitions []*Partition
}

// NewPartitions returns an empty partition slice.
func NewPartitions(logger *logger.Logger) *Partitions {
	return &Partitions{logger: logger, partitions: nil}
}

// AppendPartitions appends the input slice to the in-memory partitions.
func (p *Partitions) AppendPartitions(partitions []*Partition) {
	p.partitions = append(p.partitions, partitions...)
}

// GetSlice returns the slice of partitions.
func (p *Partitions) GetSlice() []*Partition {
	return p.partitions
}

// Drop will call a drop collection on the partitions namespace.
func Drop(ctx context.Context, logger *logger.Logger, retryer retry.Retryer, dstClient util.DestinationClient) error {
	col := dstClient.Database(constants.MongosyncDB).Collection(constants.PartitionsColl)
	err := retryer.RunForTransientErrorsOnly(
		ctx,
		logger,
		func(ri *retry.Info) error {
			ri.Log(logger.Logger, "drop", "destination", constants.MongosyncDB, constants.PartitionsColl, "dropping partitions namespace")
			return col.Drop(ctx)
		})

	if err != nil {
		return errors.Wrapf(err, "error dropping namespace %v.%v", constants.MongosyncDB, constants.PartitionsColl)
	}
	return nil
}

// GetAssignedPartitions issues a find to get all partitions assigned to this mongosync and loads it into memory.
func GetAssignedPartitions(ctx context.Context, mongosyncID string, logger *logger.Logger, retryer retry.Retryer, dstClient util.DestinationClient) (*Partitions, error) {
	col := dstClient.Database(constants.MongosyncDB).Collection(constants.PartitionsColl)
	var partitions *Partitions
	err := retryer.RunForTransientErrorsOnly(ctx, logger, func(ri *retry.Info) error {
		ri.Log(logger.Logger, "find", "destination", constants.MongosyncDB, constants.PartitionsColl, "Getting partition info from database.")
		partitions = NewPartitions(logger)
		// Find all partitions assigned to mongosyncId.
		cursor, driverErr := col.Find(ctx, bson.D{primitive.E{"_id.id", mongosyncID}})
		if driverErr != nil {
			return errors.Wrapf(driverErr, "failed to call find on destination to get partition info")
		}

		defer cursor.Close(ctx)
		for cursor.Next(ctx) {
			p := Partition{}
			decodeErr := cursor.Decode(&p)
			if decodeErr != nil {
				return errors.Wrapf(decodeErr, "failed to decode partition")
			}
			partitions.AppendPartitions([]*Partition{&p})

			ri.IterationSuccess()
		}

		return cursor.Err()
	})

	if err != nil {
		return nil, errors.Wrapf(err, "failed to get partition info from destination")
	}
	return partitions, err
}

// Persist persists all entries in the partitions map to the destination cluster.
// This must be called within a transaction in order to make sure we persist all or none of the entries.
// We explicitly do not use RunRetryableFunc since this is called inside a transaction.
// See: https://github.com/mongodb/specifications/blob/master/source/transactions/transactions.rst#interaction-with-retryable-writes
func (p *Partitions) Persist(ctx context.Context, client util.DestinationClient) error {
	col := client.Database(constants.MongosyncDB).Collection(constants.PartitionsColl)

	for _, p := range p.partitions {
		bytes, err := bson.Marshal(p)
		if err != nil {
			return errors.Wrapf(err, "failed to marshal partition")
		}
		_, err = col.InsertOne(ctx, bytes)
		if err != nil {
			return errors.Wrapf(err, "failed to perform insert of partition with sourceUUID '%s'", p.Key.SourceUUID.String())
		}
	}

	return nil
}

// GetGlobalLockWriteMask gets the write mask used by global lock to syncronize the access to Persist().
func (p *Partitions) GetGlobalLockWriteMask() int {
	// Partitions state is not protected by the global lock.
	return 0
}

type minOrMaxBound string

const (
	minBound minOrMaxBound = "min"
	maxBound minOrMaxBound = "max"
)

// PartitionCollection splits the source collection into one or more partitions. These partitions are
// expected to be somewhat similar in size, but this is never guaranteed.
//
// For example, if we split a collection of documents with _id values from 1 to 100 into 4 partitions,
// then the partitions may look something like [1, 17], [17, 39], [39, 78], [78, 100]. For smaller
// collections resulting in only one partition, the partition will be [1, 100].
func PartitionCollection(ctx context.Context, uuidEntry *globalstate.UUIDEntry, retryer retry.Retryer, srcClient util.SourceClient, replicatorList []globalstate.Replicator, subLogger *logger.Logger) ([]*Partition, error) {
	return PartitionCollectionWithParameters(ctx, uuidEntry, retryer, srcClient, replicatorList, defaultSampleRate, defaultSampleMinNumDocs, defaultPartitionSizeInBytes, subLogger)
}

// PartitionCollectionWithParameters is the implementation for PartitionCollection. It is only directly used in integration tests.
func PartitionCollectionWithParameters(ctx context.Context, uuidEntry *globalstate.UUIDEntry, retryer retry.Retryer, srcClient util.SourceClient, replicatorList []globalstate.Replicator, sampleRate float64, sampleMinNumDocs int, partitionSizeInBytes int64, subLogger *logger.Logger) ([]*Partition, error) {
	// Get the source collection.
	srcDB := srcClient.Database(uuidEntry.DBName)
	srcColl := srcDB.Collection(uuidEntry.SrcCollName)

	// Get the collection's size in bytes and its document count. It is okay if these return zero since there might still be
	// items in the collection. Rely on getOuterIDBound to do a majority read to determine if we continue processing the collection.
	collSizeInBytes, collDocCount, err := GetSizeAndDocumentCount(ctx, subLogger, retryer, srcColl, uuidEntry.SrcUUID)
	if err != nil {
		return nil, err
	}

	// The lower bound for the collection. There is no partitioning to do if the bound is nil.
	minIDBound, err := getOuterIDBound(ctx, subLogger, retryer, minBound, srcDB, uuidEntry.SrcCollName, uuidEntry.SrcUUID)
	if err != nil {
		return nil, err
	}
	if minIDBound == nil {
		subLogger.Info().Msgf("No minimum _id found for collection %s.%s; will not perform collection copy for this collection.", uuidEntry.DBName, uuidEntry.SrcCollName)
		return nil, nil
	}

	// The upper bound for the collection. There is no partitioning to do if the bound is nil.
	maxIDBound, err := getOuterIDBound(ctx, subLogger, retryer, maxBound, srcDB, uuidEntry.SrcCollName, uuidEntry.SrcUUID)
	if err != nil {
		return nil, err
	}
	if maxIDBound == nil {
		subLogger.Info().Msgf("No maximum _id found for collection %s.%s; will not perform collection copy for this collection.", uuidEntry.DBName, uuidEntry.SrcCollName)
		return nil, nil
	}

	// The total number of partitions needed for the collection. If it is a capped collection, we
	// must only create one partition for the entire collection. Otherwise, calculate the
	// appropriate number of partitions.
	numPartitions := 1
	isCapped := uuidEntry.CappedSpec != nil

	if !isCapped {
		numPartitions = getNumPartitions(collSizeInBytes, partitionSizeInBytes)
	}

	// Prepend the lower bound and append the upper bound to any intermediate bounds.
	allIDBounds := make([]interface{}, 0, numPartitions+1)
	allIDBounds = append(allIDBounds, minIDBound)

	// The intermediate bounds for the collection (i.e. all bounds apart from the lower and upper bounds).
	// It's okay for these bounds to be nil, since we already have the lower and upper bounds from which
	// to make at least one partition.
	midIDBounds, collDropped, err := getMidIDBounds(ctx, subLogger, retryer, srcDB, uuidEntry.SrcCollName, uuidEntry.SrcUUID, collDocCount, numPartitions, sampleMinNumDocs, sampleRate)
	if err != nil {
		return nil, err
	}
	if collDropped {
		// Skip this collection.
		return nil, nil
	}
	if midIDBounds != nil {
		allIDBounds = append(allIDBounds, midIDBounds...)
	}

	allIDBounds = append(allIDBounds, maxIDBound)

	if len(allIDBounds) < 2 {
		return nil, errors.Errorf("need at least 2 _id bounds to make a partition, but got %d _id bound(s)", len(allIDBounds))
	}

	// TODO (REP-552): Figure out what situations this occurs for, and whether or not it results from a bug.
	if len(allIDBounds) != numPartitions+1 {
		subLogger.Info().Msgf("Number of _id bounds (%d) is not 1 greater than expected number of partitions (%d).", len(allIDBounds), numPartitions)
	}

	// Choose a random index to start to avoid over-assigning partitions to a specific replicator.
	// rand.Int() generates non-negative integers only.
	replIndex := rand.Int() % len(replicatorList)
	subLogger.Info().Msgf("Creating %d partitions for collection %s.%s, isCappedColl: %t", len(allIDBounds)-1, uuidEntry.DBName, uuidEntry.SrcCollName, isCapped)

	// Create the partitions with the index key bounds.
	partitions := make([]*Partition, 0, len(allIDBounds)-1)

	for i := 0; i < len(allIDBounds)-1; i++ {
		partitionKey := PartitionKey{
			SourceUUID:  uuidEntry.SrcUUID,
			MongosyncID: replicatorList[replIndex].ID,
			Lower:       allIDBounds[i],
		}
		partition := &Partition{
			Key:      partitionKey,
			Phase:    phase.PartitionNotStarted,
			Ns:       &Namespace{uuidEntry.DBName, uuidEntry.SrcCollName},
			Upper:    allIDBounds[i+1],
			IsCapped: isCapped,
		}
		partitions = append(partitions, partition)

		replIndex = (replIndex + 1) % len(replicatorList)
	}

	return partitions, nil
}

// GetSizeAndDocumentCount uses collStats to return a collection's byte size and document count, in that order.
//
// Exported for usage in integration tests.
func GetSizeAndDocumentCount(ctx context.Context, logger *logger.Logger, retryer retry.Retryer, srcColl *mongo.Collection, collUUID util.UUID) (int64, int64, error) {
	srcDB := srcColl.Database()
	collName := srcColl.Name()

	value := struct {
		Size  int64 `bson:"size"`
		Count int64 `bson:"count"`
	}{}

	currCollName, err := retryer.RunForUUIDAndTransientErrors(ctx, logger, collName, func(ri *retry.Info, collectionName string) error {
		ri.Log(logger.Logger, "collStats", "source", srcDB.Name(), collectionName, "Retrieving collection size and document count.")
		request := bson.D{
			{"aggregate", collectionName},
			{"collectionUUID", collUUID},
			{"readConcern", bson.D{{"level", "majority"}}},
			{"pipeline", bson.A{
				bson.D{{"$collStats", bson.D{
					{"storageStats", bson.E{"scale", 1}},
				}}},
				bson.D{{"$addFields", bson.D{{"count", "$storageStats.count"}}}},
				bson.D{{"$addFields", bson.D{{"size", "$storageStats.size"}}}},
				bson.D{{"$project", bson.D{{"ns", 1}, {"count", 1}, {"size", 1}}}},
			}},
			{"cursor", bson.D{}},
		}

		cursor, driverErr := srcDB.RunCommandCursor(ctx, request)
		if driverErr != nil {
			return driverErr
		}

		defer cursor.Close(ctx)
		if cursor.Next(ctx) {
			if err := cursor.Decode(&value); err != nil {
				return errors.Wrapf(err, "failed to decode $collStats response for source namespace %s.%s", srcDB.Name(), collName)
			}
		}
		return nil
	})

	// TODO (REP-960): remove this check.
	// If we get NamespaceNotFoundError then return 0,0 since we won't do any partitioning with those returns
	// and the aggregation did not fail so we do not want to return an error. A
	// NamespaceNotFoundError can happen if the database does not exist.
	if util.IsNamespaceNotFoundError(err) {
		return 0, 0, nil
	}

	if err != nil {
		return 0, 0, errors.Wrapf(err, "failed to run aggregation for $collStats for source namespace %s.%s", srcDB.Name(), collName)
	}

	// CollectionUUIDMismatch where the collection does not exist will return a nil cursor and nil
	// error.
	if currCollName == "" {
		// Return 0, 0, nil as CollectionUUIDMismatch should not cause an initial sync error.
		return 0, 0, nil
	}

	return value.Size, value.Count, nil
}

// getNumPartitions returns the total number of partitions needed for the collection.
//
// The returned number is always 1 or greater, where 1 indicates that the collection
// can be represented with 1 partition and no additional splitting is needed.
func getNumPartitions(collSizeInBytes, partitionSizeInBytes int64) int {
	// Get the number of partitions as a float.
	numPartitions := float64(collSizeInBytes) / float64(partitionSizeInBytes)

	// We take the ceiling of the numPartitions needed, in order to honor the defaultPartitionSizeInBytes.
	//
	// E.g. if our collection is 1000 MB and the defaultPartitionSizeInBytes is 400 MB,
	// we'd need 1000 MB / 400 MB = 2.5 partitions for it. If we take the floor, we'd
	// end up with 1000 MB / 2 partitions = 500 MB / partition. But if we instead take
	// the ceiling, we'd end up with 1000 MB / 3 partitions = 333 MB / partition.
	return int(numPartitions) + 1
}

// getOuterIDBound returns either the smallest or largest _id value in a collection. The minOrMaxBound parameter can be set to "min" or "max" to get either, respectively.
func getOuterIDBound(ctx context.Context, subLogger *logger.Logger, retryer retry.Retryer, minOrMaxBound minOrMaxBound, srcDB util.SourceDatabase, collName string, collUUID util.UUID) (interface{}, error) {
	// Choose a sort direction based on the minOrMaxBound.
	var sortDirection int
	switch minOrMaxBound {
	case minBound:
		sortDirection = 1
	case maxBound:
		sortDirection = -1
	default:
		return nil, errors.Errorf("unknown minOrMaxBound parameter '%v' when getting outer _id bound", minOrMaxBound)
	}

	var docID interface{}
	// Get one document containing only the smallest or largest _id value in the collection.
	currCollName, err := retryer.RunForUUIDAndTransientErrors(ctx, subLogger, collName, func(ri *retry.Info, collName string) error {
		ri.Log(subLogger.Logger, "aggregate", "source", srcDB.Name(), collName, fmt.Sprintf("getting %s _id partition bound", minOrMaxBound))
		cursor, cmdErr :=
			srcDB.RunCommandCursor(ctx, bson.D{
				{"aggregate", collName},
				{"collectionUUID", collUUID},
				{"readConcern", bson.D{{"level", "majority"}}},
				{"pipeline", bson.A{
					bson.D{{"$sort", bson.D{{"_id", sortDirection}}}},
					bson.D{{"$project", bson.D{{"_id", 1}}}},
					bson.D{{"$limit", 1}},
				}},
				{"hint", bson.D{{"_id", 1}}},
				{"cursor", bson.D{}},
			})

		if cmdErr != nil {
			return cmdErr
		}

		// If we don't have at least one document, the collection is either empty or was dropped.
		defer cursor.Close(ctx)
		if !cursor.Next(ctx) {
			return nil
		}

		// Return the _id value from that document.
		docID, cmdErr = cursor.Current.LookupErr("_id")
		return cmdErr
	})

	if err != nil {
		return nil, errors.Wrapf(err, "could not get %s _id bound for source collection '%s.%s', UUID %s", minOrMaxBound, srcDB.Name(), collName, collUUID.String())
	}

	if currCollName == "" {
		subLogger.Info().Msgf("Not getting %s _id bound for source collection '%s.%s', UUID %s, because it was dropped", minOrMaxBound, srcDB.Name(), collName, collUUID.String())
		return nil, nil
	}

	return docID, nil

}

// getMidIDBounds performs a $sample and $bucketAuto aggregation on a collection and returns a slice of pseudo-randomly spaced out _id bounds.
// The number of bounds returned is: numPartitions - 1.
//
// A nil slice is returned if the collDocCount doesn't meet the sampleMinNumDocs, or if the numPartitions is less than 2.
func getMidIDBounds(ctx context.Context, logger *logger.Logger, retryer retry.Retryer, srcDB util.SourceDatabase, collName string, collUUID util.UUID, collDocCount int64, numPartitions, sampleMinNumDocs int, sampleRate float64) ([]interface{}, bool, error) {
	// We entirely avoid sampling for mid bounds if we don't meet the criteria for the number of documents or partitions.
	if collDocCount < int64(sampleMinNumDocs) || numPartitions < 2 {
		return nil, false, nil
	}

	// We sample the lesser of 4% of a collection, or 10x the number of partitions.
	// See the constant definitions at the top of this file for rationale.
	numDocsToSample := int64(sampleRate * float64(collDocCount))
	if numDocsToSample > int64(defaultMaxNumDocsToSamplePerPartition*numPartitions) {
		numDocsToSample = int64(defaultMaxNumDocsToSamplePerPartition * numPartitions)
	}

	// INTEGRATION TEST ONLY. We sample all docs in a collection
	// to perform a collection scan and get deterministic results.
	if sampleRate == 1.0 {
		numDocsToSample = collDocCount
	}

	logger.Info().Msgf("Sampling %d documents to make %d partitions for collection '%s.%s', UUID %s", numDocsToSample, numPartitions, srcDB.Name(), collName, collUUID.String())

	// Get a cursor for the $sample and $bucketAuto aggregation.
	var midIDBounds []interface{}
	currCollName, err := retryer.RunForUUIDAndTransientErrors(ctx, logger, collName, func(ri *retry.Info, collName string) error {
		ri.Log(logger.Logger, "aggregate", "source", srcDB.Name(), collName, "Retrieving mid _id partition bounds using $sample.")
		cursor, cmdErr :=
			srcDB.RunCommandCursor(ctx, bson.D{
				{"aggregate", collName},
				{"collectionUUID", collUUID},
				{"pipeline", bson.A{
					bson.D{{"$sample", bson.D{{"size", numDocsToSample}}}},
					bson.D{{"$project", bson.D{{"_id", 1}}}},
					bson.D{{"$bucketAuto",
						bson.D{
							{"groupBy", "$_id"},
							{"buckets", numPartitions},
						}}},
				}},
				{"allowDiskUse", true},
				{"cursor", bson.D{}},
			})

		if cmdErr != nil {
			return errors.Wrapf(cmdErr, "failed to $sample and $bucketAuto documents for source namespace '%s.%s', UUID %s", srcDB.Name(), collName, collUUID.String())
		}

		defer cursor.Close(ctx)

		// Iterate through all $bucketAuto documents of the form:
		// {
		//   "_id" : {
		//     "min" : ... ,
		//     "max" : ...
		//   },
		//   "count" : ...
		// }
		midIDBounds = make([]interface{}, 0, numPartitions)
		for cursor.Next(ctx) {
			// Get a mid _id bound using the $bucketAuto document's max value.
			bucketAutoDoc := make(bson.Raw, len(cursor.Current))
			copy(bucketAutoDoc, cursor.Current)
			bound, err := bucketAutoDoc.LookupErr("_id", "max")
			if err != nil {
				return errors.Wrapf(err, "failed to lookup '_id.max' key in $bucketAuto document for source namespace '%s.%s', UUID %s", srcDB.Name(), collName, collUUID.String())
			}

			// Append the copied bound to the other mid _id bounds.
			midIDBounds = append(midIDBounds, bound)
			ri.IterationSuccess()
		}

		return cursor.Err()
	})

	if err != nil {
		return nil, false, errors.Wrapf(err, "encountered a problem in the cursor when trying to $sample and $bucketAuto aggregation for source namespace '%s.%s', UUID %s", srcDB.Name(), collName, collUUID.String())
	}

	if currCollName == "" {
		// The collection was dropped on the source, so we return a nil mid ID bound.
		return nil, true, nil
	}

	if len(midIDBounds) == 0 {
		return nil, false, nil
	}

	// We remove the last $bucketAuto max value, since it does not qualify as a mid-bound.
	return midIDBounds[:len(midIDBounds)-1], false, nil
}