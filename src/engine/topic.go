package engine

import (
	"encoding/binary"
	"github.com/Chendemo12/synshare-mq/src/proto"
	"sync"
	"time"
)

var cmp = proto.NewCMPool()
var framePool = proto.NewFramePool()

type Event struct {
	q chan struct{}
}

func (e *Event) IsSet() bool { return len(e.q) > 0 }
func (e *Event) Set()        { e.q <- struct{}{} }
func (e *Event) Wait()       { <-e.q }

func NewEvent() *Event { return &Event{q: make(chan struct{})} }

type Topic struct {
	Name           []byte               // 唯一标识
	BufferSize     int                  // 生产者消息缓冲区大小
	offset         uint64               // 当前数据偏移量
	counter        *proto.Counter       // 生产者消息计数器,用于计算数据偏移量
	consumers      *sync.Map            // 全部消费者: {addr: Consumer}
	consumerQueue  chan *proto.CMessage // 等待消费者消费的数据, 若生产者消息发生了挤压,则会丢弃最早的未消费的数据
	publisherQueue *proto.Queue         // 生产者生产的数据: proto.Queue[*proto.CMessage]
	// TODO: 移除通道 consumerQueue, 由计时器和 publisherEvent 事件触发,
	// 一次性将队列里的所有数据全部发送给消费者
	publisherEvent *Event
	mu             *sync.Mutex
}

func (t *Topic) makeOffset() uint64 {
	t.offset = t.counter.ValueBeforeIncrement()
	return t.offset
}

func (t *Topic) msgMove() {
	for {
		t.publisherEvent.Wait()

		msg := t.publisherQueue.Front()
		if msg != nil {
			t.consumerQueue <- msg.(*proto.CMessage)
		}
	}
}

func (t *Topic) AddConsumer(con *Consumer) {
	t.consumers.Store(con.Addr, con)
}

func (t *Topic) GetConsumer(addr string) *Consumer {
	v, ok := t.consumers.Load(addr)
	if !ok {
		return nil
	} else {
		return v.(*Consumer)
	}
}

func (t *Topic) RemoveConsumer(addr string) {
	t.consumers.Delete(addr)
}

// Publisher 发布消费者消息,此处会将来自生产者的消息转换成消费者消息
func (t *Topic) Publisher(pm *proto.PMessage) uint64 {
	offset := t.makeOffset()
	cm := cmp.Get()

	binary.BigEndian.PutUint64(cm.Offset, offset)
	binary.BigEndian.PutUint64(cm.ProductTime, uint64(time.Now().Unix()))
	cm.Pm = pm

	// 若缓冲区已满, 则丢弃最早的数据
	t.publisherQueue.Append(cm)
	t.publisherEvent.Set()

	return offset
}

func (t *Topic) Consume() {
	var stream []byte
	var err error

	for msg := range t.consumerQueue {
		frame := framePool.Get()
		frame.Type = proto.CMessageType

		stream, err = frame.WriteC(msg)
		if err != nil {
			continue
		}

		t.consumers.Range(func(key, value any) bool {
			cons, ok := value.(*Consumer)
			if !ok {
				return true
			}
			go func() {
				// TODO: tcp.Remote 实现 WriteFrom(reader io.Reader) 方法
				_, _ = cons.Conn.Write(stream)
				_ = cons.Conn.Drain()
			}()

			return true
		})

		framePool.Put(frame)
	}
}

func NewTopic(name []byte, bufferSize int) *Topic {
	t := &Topic{
		Name:           name,
		BufferSize:     bufferSize,
		offset:         0,
		counter:        proto.NewCounter(),
		consumers:      &sync.Map{},
		consumerQueue:  make(chan *proto.CMessage, bufferSize),
		publisherQueue: proto.NewQueue(bufferSize),
		publisherEvent: NewEvent(),
		mu:             &sync.Mutex{},
	}
	go t.msgMove()
	go t.Consume()

	return t
}
