package kafkahelper

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/config"
	"github.com/mailgun/kafka-pixy/offsetmgr"
	"github.com/mailgun/kafka-pixy/testhelpers"
	log "github.com/sirupsen/logrus"
	"github.com/wvanbergen/kazoo-go"
	. "gopkg.in/check.v1"
)

type T struct {
	cfg      *config.Proxy
	ns       *actor.Descriptor
	c        *C
	kazooClt *kazoo.Kazoo
	kafkaClt sarama.Client
	producer sarama.AsyncProducer
	consumer sarama.Consumer
}

func New(c *C) *T {
	kh := &T{ns: actor.Root().NewChild("kh"), c: c}
	cfg := testhelpers.NewTestProxyCfg("kh").SaramaClientCfg()
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Flush.Messages = 1
	cfg.Producer.Flush.Frequency = 10 * time.Millisecond
	cfg.Producer.Return.Successes = true
	cfg.Producer.Return.Errors = true
	err := error(nil)
	if kh.kazooClt, err = kazoo.NewKazoo(testhelpers.ZookeeperPeers, kazoo.NewConfig()); err != nil {
		panic(err)
	}
	if kh.kafkaClt, err = sarama.NewClient(testhelpers.KafkaPeers, cfg); err != nil {
		panic(err)
	}
	if kh.consumer, err = sarama.NewConsumerFromClient(kh.kafkaClt); err != nil {
		panic(err)
	}
	if kh.producer, err = sarama.NewAsyncProducerFromClient(kh.kafkaClt); err != nil {
		panic(err)
	}
	return kh
}

func (kh *T) KazooClt() *kazoo.Kazoo {
	return kh.kazooClt
}

func (kh *T) KafkaClt() sarama.Client {
	return kh.kafkaClt
}

func (kh *T) Close() {
	kh.kazooClt.Close()
	kh.producer.Close()
	kh.consumer.Close()
	kh.kafkaClt.Close()
}

func (kh *T) GetNewestOffsets(topic string) []int64 {
	offsets := []int64{}
	partitions, err := kh.kafkaClt.Partitions(topic)
	if err != nil {
		panic(err)
	}
	for _, p := range partitions {
		offset, err := kh.kafkaClt.GetOffset(topic, p, sarama.OffsetNewest)
		if err != nil {
			panic(err)
		}
		offsets = append(offsets, offset)
	}
	return offsets
}

func (kh *T) GetOldestOffsets(topic string) []int64 {
	offsets := []int64{}
	partitions, err := kh.kafkaClt.Partitions(topic)
	if err != nil {
		panic(err)
	}
	for _, p := range partitions {
		offset, err := kh.kafkaClt.GetOffset(topic, p, sarama.OffsetOldest)
		if err != nil {
			panic(err)
		}
		offsets = append(offsets, offset)
	}
	return offsets
}

func (kh *T) GetMessages(topic string, begin, end []int64) [][]string {
	writtenMsgs := make([][]string, len(begin))
	for i := range begin {
		p, err := kh.consumer.ConsumePartition(topic, int32(i), begin[i])
		if err != nil {
			panic(err)
		}
		writtenMsgCount := int(end[i] - begin[i])
		for j := 0; j < writtenMsgCount; j++ {
			connMsg := <-p.Messages()
			writtenMsgs[i] = append(writtenMsgs[i], string(connMsg.Value))
		}
		p.Close()
	}
	return writtenMsgs
}

func (kh *T) PutMessages(prefix, topic string, keys map[string]int) map[string][]*sarama.ProducerMessage {
	messages := make(map[string][]*sarama.ProducerMessage)
	var wg sync.WaitGroup
	total := 0
	for key, count := range keys {
		total += count
		wg.Add(1)
		go func(key string, count int) {
			defer wg.Done()
			for i := 0; i < count; i++ {
				message := fmt.Sprintf("%s:%s:%d", prefix, key, i)
				keyEncoder := sarama.StringEncoder(key)
				msgEncoder := sarama.StringEncoder(message)
				prodMsg := &sarama.ProducerMessage{
					Topic: topic,
					Key:   keyEncoder,
					Value: msgEncoder,
				}
				kh.producer.Input() <- prodMsg
			}
		}(key, count)
	}
	for i := 0; i < total; i++ {
		select {
		case prodMsg := <-kh.producer.Successes():
			key := string(prodMsg.Key.(sarama.StringEncoder))
			messages[key] = append(messages[key], prodMsg)
			log.Infof("*** produced: topic=%s, partition=%d, offset=%d, message=%s",
				topic, prodMsg.Partition, prodMsg.Offset, prodMsg.Value)
		case prodErr := <-kh.producer.Errors():
			kh.c.Error(prodErr)
		}
	}
	// Sort the produced messages in ascending order of their offsets.
	for _, keyMessages := range messages {
		sort.Slice(keyMessages, func(i, j int) bool { return keyMessages[i].Offset < keyMessages[j].Offset })
	}
	wg.Wait()
	return messages
}

func (kh *T) ResetOffsets(group, topic string) {
	cfg := config.DefaultProxy()
	cfg.Consumer.OffsetsCommitInterval = 1 * time.Millisecond
	cfg.Consumer.OffsetsCommitInterval = 1 * time.Millisecond
	omf := offsetmgr.SpawnFactory(kh.ns, cfg, kh.kafkaClt)
	defer omf.Stop()
	partitions, err := kh.kafkaClt.Partitions(topic)
	kh.c.Assert(err, IsNil)
	var wg sync.WaitGroup
	for _, p := range partitions {
		wg.Add(1)
		go func(p int32) {
			defer wg.Done()
			offset, err := kh.kafkaClt.GetOffset(topic, p, sarama.OffsetNewest)
			kh.c.Assert(err, IsNil)
			om, err := omf.Spawn(kh.ns, group, topic, p)
			kh.c.Assert(err, IsNil)
			om.SubmitOffset(offsetmgr.Offset{Val: offset, Meta: ""})
			log.Infof("*** set initial offset %s/%s/%d=%d", group, topic, p, offset)
			om.Stop()
		}(p)
	}
	wg.Wait()
}

func (kh *T) SetOffsetValues(group, topic string, offsetValues []int64) {
	offsets := make([]offsetmgr.Offset, len(offsetValues))
	for i, offsetVal := range offsetValues {
		offsets[i] = offsetmgr.Offset{Val: offsetVal}
	}
	kh.SetOffsets(group, topic, offsets)
}

func (kh *T) SetOffsets(group, topic string, offsets []offsetmgr.Offset) {
	cfg := config.DefaultProxy()
	cfg.Consumer.OffsetsCommitInterval = 1 * time.Millisecond
	omf := offsetmgr.SpawnFactory(kh.ns, cfg, kh.kafkaClt)
	defer omf.Stop()
	partitions, err := kh.kafkaClt.Partitions(topic)
	kh.c.Assert(err, IsNil)
	var wg sync.WaitGroup
	for _, p := range partitions {
		wg.Add(1)
		go func(p int32) {
			defer wg.Done()
			om, err := omf.Spawn(kh.ns, group, topic, p)
			kh.c.Assert(err, IsNil)
			om.SubmitOffset(offsets[p])
			log.Infof("*** set initial offset %s/%s/%d=%+v", group, topic, p, offsets[p])
			om.Stop()
		}(p)
	}
	wg.Wait()
}

func (kh *T) GetCommittedOffsets(group, topic string) []offsetmgr.Offset {
	cfg := config.DefaultProxy()
	cfg.Consumer.OffsetsCommitInterval = 1 * time.Millisecond
	omf := offsetmgr.SpawnFactory(kh.ns, cfg, kh.kafkaClt)
	defer omf.Stop()
	partitions, err := kh.kafkaClt.Partitions(topic)
	kh.c.Assert(err, IsNil)
	offsets := make([]offsetmgr.Offset, len(partitions))
	var wg sync.WaitGroup
	for _, p := range partitions {
		wg.Add(1)
		go func(p int32) {
			defer wg.Done()
			om, err := omf.Spawn(kh.ns, group, topic, p)
			kh.c.Assert(err, IsNil)
			offsets[p] = <-om.CommittedOffsets()
			om.Stop()
		}(p)
	}
	wg.Wait()
	return offsets
}
