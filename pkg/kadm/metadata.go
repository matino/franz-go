package kadm

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// TopicID is the 16 byte underlying topic ID.
type TopicID [16]byte

// String returns the topic ID encoded as base64.
func (t TopicID) String() string { return base64.StdEncoding.EncodeToString(t[:]) }

// MarshalJSON returns the topic ID encoded as quoted base64.
func (t TopicID) MarshalJSON() ([]byte, error) { return []byte(`"` + t.String() + `"`), nil }

// PartitionDetail is the detail of a partition as returned by a metadata
// response. If the partition fails to load / has an error, then only the
// partition number itself and the Err fields will be set.
type PartitionDetail struct {
	Topic     string // Topic is the topic this partition belongs to.
	Partition int32  // Partition is the partition number these details are for.

	Leader          int32   // Leader is the broker leader, if there is one, otherwise -1.
	LeaderEpoch     int32   // LeaderEpoch is the leader's current epoch.
	Replicas        []int32 // Replicas is the list of replicas.
	ISR             []int32 // ISR is the list of in sync replicas.
	OfflineReplicas []int32 // OfflineReplicas is the list of offline replicas.

	Err error // Err is non-nil if the partition currently has a load error.
}

// PartitionDetails contains details for partitions as returned by a metadata
// response.
type PartitionDetails map[int32]PartitionDetail

// Sorted returns the partitions in sorted order.
func (ds PartitionDetails) Sorted() []PartitionDetail {
	s := make([]PartitionDetail, 0, len(ds))
	for _, d := range ds {
		s = append(s, d)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Partition < s[j].Partition })
	return s
}

// Numbers returns a sorted list of all partition numbers.
func (ds PartitionDetails) Numbers() []int32 {
	all := make([]int32, 0, len(ds))
	for p := range ds {
		all = append(all, p)
	}
	return int32s(all)
}

// TopicDetail is the detail of a topic as returned by a metadata response. If
// the topic fails to load / has an error, then there will be no partitions.
type TopicDetail struct {
	Topic string // Topic is the topic these details are for.

	ID         TopicID          // TopicID is the topic's ID, or all 0 if the broker does not support IDs.
	IsInternal bool             // IsInternal is whether the topic is an internal topic.
	Partitions PartitionDetails // Partitions contains details about the topic's partitions.

	Err error // Err is non-nil if the topic could not be loaded.
}

// TopicDetails contains details for topics as returned by a metadata response.
type TopicDetails map[string]TopicDetail

// Topics returns a sorted list of all topic names.
func (ds TopicDetails) Names() []string {
	all := make([]string, 0, len(ds))
	for t := range ds {
		all = append(all, t)
	}
	sort.Strings(all)
	return all
}

// Sorted returns all topics in sorted order.
func (ds TopicDetails) Sorted() []TopicDetail {
	s := make([]TopicDetail, 0, len(ds))
	for _, d := range ds {
		s = append(s, d)
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].Topic == "" {
			if s[j].Topic == "" {
				return bytes.Compare(s[i].ID[:], s[j].ID[:]) == -1
			}
			return true
		}
		if s[j].Topic == "" {
			return false
		}
		return s[i].Topic < s[j].Topic
	})
	return s
}

// Has returns whether the topic details has the given topic and, if so, that
// the topic's load error is not an unknown topic error.
func (ds TopicDetails) Has(topic string) bool {
	d, ok := ds[topic]
	return ok && d.Err != kerr.UnknownTopicOrPartition
}

// FilterInternal deletes any internal topics from this set of topic details.
func (ds TopicDetails) FilterInternal() {
	for t, d := range ds {
		if d.IsInternal {
			delete(ds, t)
		}
	}
}

// Metadata is the data from a metadata response.
type Metadata struct {
	Cluster    string        // Cluster is the cluster name, if any.
	Controller int32         // Controller is the node ID of the controller broker, if available, otherwise -1.
	Brokers    BrokerDetails // Brokers contains broker details.
	Topics     TopicDetails  // Topics contains topic details.
}

func int32s(is []int32) []int32 {
	sort.Slice(is, func(i, j int) bool { return is[i] < is[j] })
	return is
}

// ListBrokers issues a metadata request and returns BrokerDetails. This
// returns an error if the request fails to be issued, or an *AuthError.
func (cl *Client) ListBrokers(ctx context.Context) (BrokerDetails, error) {
	m, err := cl.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	return m.Brokers, nil
}

// Metadata issues a metadata request and returns it. Specific topics to
// describe can be passed as additional arguments. If no topics are specified,
// all topics are requested.
//
// This returns an error if the request fails to be issued, or an *AuthErr.
func (cl *Client) Metadata(
	ctx context.Context,
	topics ...string,
) (Metadata, error) {
	req := kmsg.NewPtrMetadataRequest()
	for _, t := range topics {
		rt := kmsg.NewMetadataRequestTopic()
		rt.Topic = kmsg.StringPtr(t)
		req.Topics = append(req.Topics, rt)
	}
	resp, err := req.RequestWith(ctx, cl.cl)
	if err != nil {
		return Metadata{}, err
	}

	tds := make(map[string]TopicDetail, len(resp.Topics))
	for _, t := range resp.Topics {
		if err := maybeAuthErr(t.ErrorCode); err != nil {
			return Metadata{}, err
		}
		td := TopicDetail{
			Topic:      *t.Topic,
			ID:         t.TopicID,
			Partitions: make(map[int32]PartitionDetail),
			IsInternal: t.IsInternal,
			Err:        kerr.ErrorForCode(t.ErrorCode),
		}
		for _, p := range t.Partitions {
			td.Partitions[p.Partition] = PartitionDetail{
				Topic:     td.Topic,
				Partition: p.Partition,

				Leader:          p.Leader,
				LeaderEpoch:     p.LeaderEpoch,
				Replicas:        int32s(p.Replicas),
				ISR:             int32s(p.ISR),
				OfflineReplicas: int32s(p.OfflineReplicas),

				Err: kerr.ErrorForCode(p.ErrorCode),
			}
		}
		tds[*t.Topic] = td
	}

	m := Metadata{
		Controller: resp.ControllerID,
		Topics:     tds,
	}
	if resp.ClusterID != nil {
		m.Cluster = *resp.ClusterID
	}

	for _, b := range resp.Brokers {
		m.Brokers = append(m.Brokers, kgo.BrokerMetadata{
			NodeID: b.NodeID,
			Host:   b.Host,
			Port:   b.Port,
			Rack:   b.Rack,
		})
	}

	if len(topics) > 0 && len(m.Topics) != len(topics) {
		return Metadata{}, fmt.Errorf("metadata returned only %d topics of %d requested", len(m.Topics), len(topics))
	}

	return m, nil
}

// ListedOffset contains record offset information.
type ListedOffset struct {
	Topic     string // Topic is the topic this offset is for.
	Partition int32  // Partition is the partition this offset is for.

	Timestamp   int64 // Timestamp is the millisecond of the offset if listing after a time, otherwise -1.
	Offset      int64 // Offset is the record offset, or -1 if one could not be found.
	LeaderEpoch int32 // LeaderEpoch is the leader epoch at this offset, if any, otherwise -1.

	Err error // Err is non-nil if the partition has a load error.
}

// ListedOffsets contains per-partition record offset information that is
// returned from any of the List.*Offsets functions.
type ListedOffsets map[string]map[int32]ListedOffset

// Each calls fn for each listed offset.
func (l ListedOffsets) Each(fn func(ListedOffset)) {
	for _, ps := range l {
		for _, o := range ps {
			fn(o)
		}
	}
}

// ListStartOffsets returns the start (oldest) offsets for each partition in
// each requested topic. In Kafka terms, this returns the log start offset. If
// no topics are specified, all topics are listed.
//
// This may return *ShardErrors.
func (cl *Client) ListStartOffsets(ctx context.Context, topics ...string) (ListedOffsets, error) {
	return cl.listOffsets(ctx, 0, -2, topics)
}

// ListEndOffsets returns the end (newest) offsets for each partition in each
// requested topic. In Kafka terms, this returns high watermarks. If no topics
// are specified, all topics are listed.
//
// This may return *ShardErrors.
func (cl *Client) ListEndOffsets(ctx context.Context, topics ...string) (ListedOffsets, error) {
	return cl.listOffsets(ctx, 0, -1, topics)
}

// ListCommittedOffsets returns newest committed offsets for each partition in
// each requested topic. A committed offset may be slightly less than the
// latest offset. In Kafka terms, committed means the last stable offset, and
// newest means the high watermark. Record offsets in active, uncommitted
// transactions will not be returned. If no topics are specified, all topics
// are listed.
//
// This may return *ShardErrors.
func (cl *Client) ListCommittedOffsets(ctx context.Context, topics ...string) (ListedOffsets, error) {
	return cl.listOffsets(ctx, 1, -1, topics)
}

// ListOffsetsAfterMilli returns the first offsets after the requested
// millisecond timestamp. Unlike listing start/end/committed offsets, offsets
// returned from this function also include the timestamp of the offset. If no
// topics are specified, all topics are listed.
//
// This may return *ShardErrors.
func (cl *Client) ListOffsetsAfterMilli(ctx context.Context, millisecond int64, topics ...string) (ListedOffsets, error) {
	return cl.listOffsets(ctx, 0, millisecond, topics)
}

func (cl *Client) listOffsets(ctx context.Context, isolation int8, timestamp int64, topics []string) (ListedOffsets, error) {
	tds, err := cl.ListTopics(ctx, topics...)
	if err != nil {
		return nil, err
	}

	req := kmsg.NewPtrListOffsetsRequest()
	req.IsolationLevel = isolation
	for t, td := range tds {
		rt := kmsg.NewListOffsetsRequestTopic()
		rt.Topic = t
		for p := range td.Partitions {
			rp := kmsg.NewListOffsetsRequestTopicPartition()
			rp.Partition = p
			rp.Timestamp = timestamp
			rt.Partitions = append(rt.Partitions, rp)
		}
		req.Topics = append(req.Topics, rt)
	}

	shards := cl.cl.RequestSharded(ctx, req)
	list := make(ListedOffsets)
	return list, shardErrEach(req, shards, func(kr kmsg.Response) error {
		resp := kr.(*kmsg.ListOffsetsResponse)
		for _, t := range resp.Topics {
			lt, ok := list[t.Topic]
			if !ok {
				lt = make(map[int32]ListedOffset)
				list[t.Topic] = lt
			}
			for _, p := range t.Partitions {
				if err := maybeAuthErr(p.ErrorCode); err != nil {
					return err
				}
				lt[p.Partition] = ListedOffset{
					Topic:       t.Topic,
					Partition:   p.Partition,
					Timestamp:   p.Timestamp,
					Offset:      p.Offset,
					LeaderEpoch: p.LeaderEpoch,
					Err:         kerr.ErrorForCode(p.ErrorCode),
				}
			}
		}
		return nil
	})
}