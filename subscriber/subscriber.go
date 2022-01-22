package subscriber

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"
)

type StreamSubscriber struct {
	dynamoSvc         *dynamodb.Client
	streamSvc         *dynamodbstreams.Client
	table             *string
	ShardIteratorType types.ShardIteratorType
	Limit             *int32
	ctx               context.Context
}

func NewStreamSubscriber(dynamoSvc *dynamodb.Client, streamSvc *dynamodbstreams.Client, table string) *StreamSubscriber {
	s := &StreamSubscriber{
		dynamoSvc: dynamoSvc,
		streamSvc: streamSvc,
		table:     &table,
		ctx:       context.Background(),
	}
	s.applyDefaults()
	return s
}

func (r *StreamSubscriber) applyDefaults() {
	if r.ShardIteratorType == "" {
		r.ShardIteratorType = types.ShardIteratorTypeLatest
	}
}

func (r *StreamSubscriber) SetLimit(v int32) {
	r.Limit = aws.Int32(v)
}

func (r *StreamSubscriber) SetShardIteratorType(s types.ShardIteratorType) {
	r.ShardIteratorType = s
}

func (r *StreamSubscriber) GetStreamData() (<-chan *types.Record, <-chan error) {

	ch := make(chan *types.Record, 1)
	errCh := make(chan error, 1)

	go func(ch chan<- *types.Record, errCh chan<- error) {
		var shardId *string
		var prevShardId *string
		var streamArn *string
		var err error

		for {
			prevShardId = shardId
			shardId, streamArn, err = r.findProperShardId(prevShardId)
			if err != nil {
				errCh <- err
			}
			if shardId != nil {
				err = r.processShardBackport(shardId, streamArn, ch)
				if err != nil {
					errCh <- err
					// reset shard id to process it again
					shardId = prevShardId
				}
			}
			if shardId == nil {
				time.Sleep(time.Second * 10)
			}

		}
	}(ch, errCh)

	return ch, errCh
}

func (r *StreamSubscriber) GetStreamDataAsync() (<-chan *types.Record, <-chan error) {
	ch := make(chan *types.Record, 1)
	errCh := make(chan error, 1)

	needUpdateChannel := make(chan struct{}, 1)
	needUpdateChannel <- struct{}{}

	allShards := make(map[string]struct{})

	shardProcessingLimit := 5
	shardsCh := make(chan *dynamodbstreams.GetShardIteratorInput, shardProcessingLimit)
	lock := sync.Mutex{}

	go func() {
		tick := time.NewTicker(time.Minute)

		for {
			select {
			case <-tick.C:
				needUpdateChannel <- struct{}{}
			case <-needUpdateChannel:
				streamArn, err := r.getLatestStreamArn()
				if err != nil {
					errCh <- err
					return
				}
				ids, err := r.getShardIds(streamArn)
				if err != nil {
					errCh <- err
					return
				}
				for _, sObj := range ids {
					lock.Lock()
					if _, ok := allShards[*sObj.ShardId]; !ok {
						allShards[*sObj.ShardId] = struct{}{}
						shardsCh <- &dynamodbstreams.GetShardIteratorInput{
							StreamArn:         streamArn,
							ShardId:           sObj.ShardId,
							ShardIteratorType: r.ShardIteratorType,
						}
					}
					lock.Unlock()
				}
			}
		}
	}()

	limit := make(chan struct{}, shardProcessingLimit)

	go func() {
		time.Sleep(time.Second * 10)
		for shardInput := range shardsCh {
			limit <- struct{}{}
			go func(sInput *dynamodbstreams.GetShardIteratorInput) {
				err := r.processShard(sInput, ch)
				if err != nil {
					errCh <- err
				}
				// TODO: think about cleaning list of shards: delete(allShards, *sInput.ShardId)
				<-limit
			}(shardInput)
		}
	}()

	return ch, errCh
}

func (r *StreamSubscriber) getShardIds(streamArn *string) (ids []types.Shard, err error) {
	des, err := r.streamSvc.DescribeStream(r.ctx, &dynamodbstreams.DescribeStreamInput{
		StreamArn: streamArn,
	})
	if err != nil {
		return nil, err
	}
	// No shards
	if len(des.StreamDescription.Shards) == 0 {
		return nil, nil
	}

	return des.StreamDescription.Shards, nil
}

func (r *StreamSubscriber) findProperShardId(previousShardId *string) (shadrId *string, streamArn *string, err error) {
	streamArn, err = r.getLatestStreamArn()
	if err != nil {
		return nil, nil, err
	}
	des, err := r.streamSvc.DescribeStream(r.ctx, &dynamodbstreams.DescribeStreamInput{
		StreamArn: streamArn,
	})
	if err != nil {
		return nil, nil, err
	}

	if len(des.StreamDescription.Shards) == 0 {
		return nil, nil, nil
	}

	if previousShardId == nil {
		shadrId = des.StreamDescription.Shards[0].ShardId
		return
	}

	for _, shard := range des.StreamDescription.Shards {
		shadrId = shard.ShardId
		if shard.ParentShardId != nil && *shard.ParentShardId == *previousShardId {
			return
		}
	}

	return
}

func (r *StreamSubscriber) getLatestStreamArn() (*string, error) {
	tableInfo, err := r.dynamoSvc.DescribeTable(r.ctx, &dynamodb.DescribeTableInput{TableName: r.table})
	if err != nil {
		return nil, err
	}
	if nil == tableInfo.Table.LatestStreamArn {
		return nil, errors.New("empty table stream arn")
	}
	return tableInfo.Table.LatestStreamArn, nil
}

func (r *StreamSubscriber) processShardBackport(shardId, lastStreamArn *string, ch chan<- *types.Record) error {
	return r.processShard(&dynamodbstreams.GetShardIteratorInput{
		StreamArn:         lastStreamArn,
		ShardId:           shardId,
		ShardIteratorType: r.ShardIteratorType,
	}, ch)
}

func (r *StreamSubscriber) processShard(input *dynamodbstreams.GetShardIteratorInput, ch chan<- *types.Record) error {
	iter, err := r.streamSvc.GetShardIterator(r.ctx, input)
	if err != nil {
		return err
	}
	if iter.ShardIterator == nil {
		return nil
	}

	nextIterator := iter.ShardIterator

	for nextIterator != nil {
		var tdae *types.TrimmedDataAccessException
		recs, err := r.streamSvc.GetRecords(r.ctx, &dynamodbstreams.GetRecordsInput{
			ShardIterator: nextIterator,
			Limit:         r.Limit,
		})
		if errors.As(err, &tdae) {
			// Trying to request data older than 24h, that's ok
			// http://docs.aws.amazon.com/dynamodbstreams/latest/APIReference/API_GetShardIterator.html -> Errors
			return nil
		}
		if err != nil {
			return err
		}

		for _, record := range recs.Records {
			ch <- &record
		}

		nextIterator = recs.NextShardIterator

		sleepDuration := time.Second

		// Nil next itarator, shard is closed
		if nextIterator == nil {
			sleepDuration = time.Millisecond * 10
		} else if len(recs.Records) == 0 {
			// Empty set, but shard is not closed -> sleep a little
			sleepDuration = time.Second * 10
		}

		time.Sleep(sleepDuration)
	}
	return nil
}
