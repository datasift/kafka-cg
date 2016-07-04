package consumergroup

import (
	"log"
	"os"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/coocood/freecache"
	"github.com/funkygao/kazoo-go"
)

// The ConsumerGroup type holds all the information for a consumer that is part
// of a consumer group. Call JoinConsumerGroup to start a consumer.
type ConsumerGroup struct {
	Logger *log.Logger

	config *Config

	consumer sarama.Consumer

	kazoo     *kazoo.Kazoo
	group     *kazoo.Consumergroup
	instance  *kazoo.ConsumergroupInstance
	consumers kazoo.ConsumergroupInstanceList

	wg             sync.WaitGroup
	singleShutdown sync.Once

	messages chan *sarama.ConsumerMessage
	errors   chan *sarama.ConsumerError
	stopper  chan struct{}

	offsetManager OffsetManager
	cacher        *freecache.Cache
}

// Connects to a consumer group, using Zookeeper for auto-discovery
func JoinConsumerGroup(name string, topics []string, zookeeper []string,
	config *Config) (cg *ConsumerGroup, err error) {
	if name == "" {
		return nil, sarama.ConfigurationError("Empty consumergroup name")
	}
	if len(topics) == 0 {
		return nil, sarama.ConfigurationError("No topics provided")
	}
	if len(zookeeper) == 0 {
		return nil, EmptyZkAddrs
	}

	if config == nil {
		config = NewConfig()
	}
	config.ClientID = name
	if err = config.Validate(); err != nil {
		return
	}

	var kz *kazoo.Kazoo
	if kz, err = kazoo.NewKazoo(zookeeper, config.Zookeeper); err != nil {
		return
	}

	var brokers []string
	brokers, err = kz.BrokerList()
	if err != nil {
		kz.Close()
		return
	}

	group := kz.Consumergroup(name)

	if config.Offsets.ResetOffsets {
		err = group.ResetOffsets()
		if err != nil {
			log.Printf("FAILED to reset offsets of consumergroup: %s!\n", err)
			kz.Close()
			return
		}
	}

	var consumer sarama.Consumer
	if consumer, err = sarama.NewConsumer(brokers, config.Config); err != nil {
		kz.Close()
		return
	}

	instance := group.NewInstance()
	cg = &ConsumerGroup{
		Logger: log.New(os.Stdout, "[KafkaConsumerGroup] ", log.Ldate|log.Ltime),

		config:   config,
		consumer: consumer,

		kazoo:    kz,
		group:    group,
		instance: instance,

		messages: make(chan *sarama.ConsumerMessage, config.ChannelBufferSize),
		errors:   make(chan *sarama.ConsumerError, config.ChannelBufferSize),
		stopper:  make(chan struct{}),
	}
	if config.NoDup {
		cg.cacher = freecache.NewCache(1 << 20) // TODO
	}

	// Register consumer group in zookeeper
	exists, err1 := cg.group.Exists()
	if err1 != nil {
		_ = consumer.Close()
		_ = kz.Close()
		return nil, err1
	}
	if !exists {
		cg.Logger.Printf("[%s/%s] consumer group in zk creating...\n", cg.group.Name, cg.shortID())

		if err := cg.group.Create(); err != nil {
			_ = consumer.Close()
			_ = kz.Close()
			return nil, err
		}
	}

	// Register itself with zookeeper: consumers/{group}/ids/{instanceId}
	// This will lead to consumer group rebalance
	if err := cg.instance.Register(topics); err != nil {
		return nil, err
	} else {
		cg.Logger.Printf("[%s/%s] consumer instance registered in zk for %+v\n", cg.group.Name,
			cg.shortID(), topics)
	}

	offsetConfig := OffsetManagerConfig{CommitInterval: config.Offsets.CommitInterval}
	cg.offsetManager = NewZookeeperOffsetManager(cg, &offsetConfig)

	go cg.consumeTopics(topics)

	return
}

// SetLogger overrides the default logger
func (cg *ConsumerGroup) SetLogger(l *log.Logger) {
	cg.Logger = l
}

// Returns a channel that you can read to obtain events from Kafka to process.
func (cg *ConsumerGroup) Messages() <-chan *sarama.ConsumerMessage {
	return cg.messages
}

// Returns a channel that you can read to obtain errors from Kafka to process.
func (cg *ConsumerGroup) Errors() <-chan *sarama.ConsumerError {
	return cg.errors
}

func (cg *ConsumerGroup) Closed() bool {
	return cg.instance == nil
}

func (cg *ConsumerGroup) Close() error {
	shutdownError := AlreadyClosing
	cg.singleShutdown.Do(func() {
		defer cg.kazoo.Close()

		cg.Logger.Printf("[%s/%s] closing...", cg.group.Name, cg.shortID())

		shutdownError = nil

		close(cg.stopper)
		cg.wg.Wait()

		if err := cg.offsetManager.Close(); err != nil {
			cg.Logger.Printf("[%s/%s] closing offset manager: %s\n", cg.group.Name, cg.shortID(), err)
		}

		if shutdownError = cg.instance.Deregister(); shutdownError != nil {
			cg.Logger.Printf("[%s/%s] de-register consumer instance: %s\n", cg.group.Name, cg.shortID(), shutdownError)
		} else {
			cg.Logger.Printf("[%s/%s] de-registered consumer instance\n", cg.group.Name, cg.shortID())
		}

		if shutdownError = cg.consumer.Close(); shutdownError != nil {
			cg.Logger.Printf("[%s/%s] closing Sarama consumer: %v\n", cg.group.Name, cg.shortID(), shutdownError)
		}

		close(cg.messages)
		close(cg.errors)

		cg.Logger.Printf("[%s/%s] closed\n", cg.group.Name, cg.shortID())

		cg.instance = nil
	})

	return shutdownError
}

func (cg *ConsumerGroup) shortID() string {
	var identifier string
	if cg.instance == nil {
		identifier = "(defunct)"
	} else {
		identifier = cg.instance.ID[len(cg.instance.ID)-12:]
	}

	return identifier
}

func (cg *ConsumerGroup) CommitUpto(message *sarama.ConsumerMessage) error {
	return cg.offsetManager.MarkAsProcessed(message.Topic, message.Partition, message.Offset)
}

func (cg *ConsumerGroup) consumeTopics(topics []string) {
	for {
		// each loop is a new rebalance process

		select {
		case <-cg.stopper:
			return
		default:
		}

		consumers, consumerChanges, err := cg.group.WatchInstances()
		if err != nil {
			// FIXME write to err chan?
			cg.Logger.Printf("[%s/%s] watch consumer instances: %s\n", cg.group.Name, cg.shortID(), err)
			return
		}

		cg.consumers = consumers

		topicConsumerStopper := make(chan struct{})
		topicChanges := make(chan struct{})

		for _, topic := range topics {
			cg.wg.Add(1)
			go cg.watchTopicChange(topic, topicConsumerStopper, topicChanges)
			go cg.consumeTopic(topic, cg.messages, cg.errors, topicConsumerStopper)
		}

		select {
		case <-cg.stopper:
			close(topicConsumerStopper) // notify all topic consumers stop
			// cg.Close will call cg.wg.Wait()
			return

		case <-consumerChanges:
			// when zk session expires, we need to re-register ephemeral znode
			//
			// how to reproduce:
			// iptables -A  OUTPUT -p tcp -m tcp --dport 2181 -j DROP # add rule
			// after 30s
			// iptables -D  OUTPUT -p tcp -m tcp --dport 2181 -j      # rm rule
			registered, err := cg.instance.Registered()
			if err != nil {
				cg.Logger.Printf("[%s/%s] %s", cg.group.Name, cg.shortID(), err)
			} else if !registered { // this sub instances was killed
				err = cg.instance.Register(topics)
				if err != nil {
					cg.Logger.Printf("[%s/%s] register consumer instance for %+v: %s\n",
						cg.group.Name, cg.shortID(), topics, err)
				} else {
					cg.Logger.Printf("[%s/%s] re-registered consumer instance for %+v\n",
						cg.group.Name, cg.shortID(), topics)
				}
			}

			cg.Logger.Printf("[%s/%s] rebalance due to %+v consumer list change\n",
				cg.group.Name, cg.shortID(), topics)
			close(topicConsumerStopper) // notify all topic consumers stop
			cg.wg.Wait()                // wait for all topic consumers finish

		case <-topicChanges:
			cg.Logger.Printf("[%s/%s] rebalance due to topic %+v change\n",
				cg.group.Name, cg.shortID(), topics)
			close(topicConsumerStopper) // notify all topic consumers stop
			cg.wg.Wait()                // wait for all topic consumers finish
		}
	}
}

// watchTopicChange watch partition changes on a topic.
func (cg *ConsumerGroup) watchTopicChange(topic string, stopper <-chan struct{}, topicChanges chan<- struct{}) {
	_, topicPartitionChanges, err := cg.kazoo.Topic(topic).WatchPartitions()
	if err != nil {
		cg.Logger.Printf("[%s/%s] topic %s: %s\n", cg.group.Name, cg.shortID(), topic, err)
		// FIXME err chan?
		return
	}

	select {
	case <-cg.stopper:
		return

	case <-stopper:
		return

	case <-topicPartitionChanges:
		close(topicChanges)
	}
}

func (cg *ConsumerGroup) consumeTopic(topic string, messages chan<- *sarama.ConsumerMessage,
	errors chan<- *sarama.ConsumerError, stopper <-chan struct{}) {
	defer cg.wg.Done()

	select {
	case <-stopper:
		return
	default:
	}

	cg.Logger.Printf("[%s/%s] try consuming topic: %s\n", cg.group.Name, cg.shortID(), topic)

	partitions, err := cg.kazoo.Topic(topic).Partitions()
	if err != nil {
		cg.Logger.Printf("[%s/%s] get topic %s partitions: %s\n", cg.group.Name, cg.shortID(), topic, err)
		cg.errors <- &sarama.ConsumerError{
			Topic:     topic,
			Partition: -1,
			Err:       err,
		}
		return
	}

	partitionLeaders, err := retrievePartitionLeaders(partitions)
	if err != nil {
		cg.Logger.Printf("[%s/%s] get leader broker of topic %s partitions: %s\n", cg.group.Name, cg.shortID(), topic, err)
		cg.errors <- &sarama.ConsumerError{
			Topic:     topic,
			Partition: -1,
			Err:       err,
		}
		return
	}

	dividedPartitions := dividePartitionsBetweenConsumers(cg.consumers, partitionLeaders)
	myPartitions := dividedPartitions[cg.instance.ID]

	cg.Logger.Printf("[%s/%s] topic %s claiming %d of %d partitions\n", cg.group.Name, cg.shortID(),
		topic, len(myPartitions), len(partitionLeaders))

	if len(myPartitions) == 0 {
		consumers := make([]string, 0, len(cg.consumers))
		partitions := make([]int32, 0, len(partitionLeaders))
		for _, c := range cg.consumers {
			consumers = append(consumers, c.ID)
		}
		for _, p := range partitionLeaders {
			partitions = append(partitions, p.id)
		}

		cg.Logger.Printf("[%s/%s] topic %s will standby, {C:%+v, P:%+v}\n",
			cg.group.Name, cg.shortID(), topic, consumers, partitions)
	}

	// Consume all the assigned partitions
	var wg sync.WaitGroup
	for _, partition := range myPartitions {
		wg.Add(1)
		go cg.consumePartition(topic, partition.ID, messages, errors, &wg, stopper)
	}

	wg.Wait()
	cg.Logger.Printf("[%s/%s] stopped consuming topic: %s\n", cg.group.Name, cg.shortID(), topic)
}

func (cg *ConsumerGroup) consumePartition(topic string, partition int32, messages chan<- *sarama.ConsumerMessage,
	errors chan<- *sarama.ConsumerError, wg *sync.WaitGroup, stopper <-chan struct{}) {
	defer wg.Done()

	select {
	case <-stopper:
		return
	default:
	}

	maxRetries := int(cg.config.Offsets.ProcessingTimeout/time.Second) + 3
	for tries := 0; tries < maxRetries; tries++ {
		if err := cg.instance.ClaimPartition(topic, partition); err == nil {
			cg.Logger.Printf("[%s/%s] %s/%d claimed owner\n", cg.group.Name, cg.shortID(), topic, partition)
			break
		} else if err == kazoo.ErrPartitionClaimedByOther && tries+1 < maxRetries {
			time.Sleep(1 * time.Second)
		} else {
			// FIXME err chan?
			cg.Logger.Printf("[%s/%s] claim %s/%d: %s\n", cg.group.Name, cg.shortID(), topic, partition, err)
			return
		}
	}
	defer func() {
		cg.Logger.Printf("[%s/%s] %s/%d de-claiming owner\n", cg.group.Name, cg.shortID(), topic, partition)
		cg.instance.ReleasePartition(topic, partition)
	}()

	nextOffset, err := cg.offsetManager.InitializePartition(topic, partition)
	if err != nil {
		cg.Logger.Printf("[%s/%s] %s/%d determine initial offset: %s\n", cg.group.Name, cg.shortID(),
			topic, partition, err)
		return
	}

	if nextOffset >= 0 {
		cg.Logger.Printf("[%s/%s] %s/%d start offset: %d\n", cg.group.Name, cg.shortID(), topic, partition, nextOffset)
	} else {
		nextOffset = cg.config.Offsets.Initial
		if nextOffset == sarama.OffsetOldest {
			cg.Logger.Printf("[%s/%s] %s/%d start offset: oldest\n", cg.group.Name, cg.shortID(), topic, partition)
		} else if nextOffset == sarama.OffsetNewest {
			cg.Logger.Printf("[%s/%s] %s/%d start offset: newest\n", cg.group.Name, cg.shortID(), topic, partition)
		}
	}

	consumer, err := cg.consumer.ConsumePartition(topic, partition, nextOffset)
	if err == sarama.ErrOffsetOutOfRange {
		// if the offset is out of range, simplistically decide whether to use OffsetNewest or OffsetOldest
		// if the configuration specified offsetOldest, then switch to the oldest available offset, else
		// switch to the newest available offset.
		if cg.config.Offsets.Initial == sarama.OffsetOldest {
			cg.Logger.Printf("[%s/%s] %s/%d O:%d %s, reset to oldest\n",
				cg.group.Name, cg.shortID(), topic, partition, nextOffset, err)

			nextOffset = sarama.OffsetOldest
		} else {
			// even when user specifies initial offset, it is reset to newest
			cg.Logger.Printf("[%s/%s] %s/%d O:%d %s, reset to newest\n",
				cg.group.Name, cg.shortID(), topic, partition, nextOffset, err)

			nextOffset = sarama.OffsetNewest
		}

		// retry the consumePartition with the adjusted offset
		consumer, err = cg.consumer.ConsumePartition(topic, partition, nextOffset)
	}
	if err != nil {
		// FIXME err chan?
		cg.Logger.Printf("[%s/%s] %s/%d: %s", cg.group.Name, cg.shortID(), topic, partition, err)
		return
	}
	defer consumer.Close()

	err = nil
	var lastOffset int64 = -1 // aka unknown
partitionConsumerLoop:
	for {
		select {
		case <-stopper:
			break partitionConsumerLoop

		case err := <-consumer.Errors():
			for {
				select {
				case errors <- err:
					continue partitionConsumerLoop

				case <-stopper:
					break partitionConsumerLoop
				}
			}

		case message := <-consumer.Messages():
			for {
				select {
				case <-stopper:
					break partitionConsumerLoop

				case messages <- message:
					if message != nil {
						lastOffset = message.Offset
						cg.offsetManager.MarkAsConsumed(topic, partition, lastOffset)
					}
					continue partitionConsumerLoop
				}
			}
		}
	}

	cg.Logger.Printf("[%s/%s] %s/%d stopping at offset: %d\n", cg.group.Name, cg.shortID(), topic, partition, lastOffset)
	if err := cg.offsetManager.FinalizePartition(topic, partition, lastOffset, cg.config.Offsets.ProcessingTimeout); err != nil {
		cg.Logger.Printf("[%s/%s] %s/%d: %s", cg.group.Name, cg.shortID(), topic, partition, err)
	}
}
