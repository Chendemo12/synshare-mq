package engine

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Chendemo12/fastapi-tool/logger"
	"github.com/Chendemo12/functools/tcp"
	"github.com/Chendemo12/synshare-mq/src/proto"
	"os"
	"os/signal"
	"sync"
	"time"
)

type Config struct {
	Host        string       `json:"host"`
	Port        string       `json:"port"`
	MaxOpenConn int          `json:"max_open_conn"`
	BufferSize  int          `json:"buffer_size"`
	Logger      logger.Iface `json:"-"`
}

type Engine struct {
	conf      *Config
	producers []*Producer // 生产者
	consumers []*Consumer // 消费者
	topics    *sync.Map
	transfer  *Transfer
}

// RangeTopic if false returned, for-loop will stop
func (e *Engine) RangeTopic(fn func(topic *Topic) bool) {
	e.topics.Range(func(key, value any) bool {
		return fn(value.(*Topic))
	})
}

func (e *Engine) AddTopic(name []byte) *Topic {
	topic := NewTopic(name, e.conf.BufferSize)
	e.topics.Store(string(name), topic)
	return topic
}

// GetTopic 获取topic,并在不存在时自动新建一个topic
func (e *Engine) GetTopic(name []byte) *Topic {
	var topic *Topic

	v, ok := e.topics.Load(string(name))
	if !ok {
		topic = e.AddTopic(name)
	} else {
		topic = v.(*Topic)
	}

	return topic
}

// GetTopicOffset 查询指定topic当前的消息偏移量
func (e *Engine) GetTopicOffset(name []byte) uint64 {
	var offset uint64

	e.RangeTopic(func(topic *Topic) bool {
		if bytes.Compare(topic.Name, name) == 0 {
			offset = topic.offset
			return false
		}
		return true
	})

	return offset
}

// RemoveConsumer 删除一个消费者
func (e *Engine) RemoveConsumer(addr string) {
	for _, consumer := range e.consumers {
		if consumer == nil || consumer.Conn == nil {
			continue
		}
		if consumer.Addr == addr {
			for _, name := range consumer.Conf.Topics {
				e.GetTopic([]byte(name)).RemoveConsumer(addr)
			}
		}
	}
}

func (e *Engine) IsProducerRegister(addr string) bool {
	for _, p := range e.producers {
		if p.Addr == addr {
			return true
		}
	}
	return false
}

// Publisher 发布消息,并返回此消息在当前topic中的偏移量
func (e *Engine) Publisher(msg *proto.PMessage) uint64 {
	return e.GetTopic(msg.Topic).Publisher(msg)
}

// ProducerInterval 允许生产者发送数据间隔
func (e *Engine) ProducerInterval() time.Duration {
	return time.Millisecond * 500
}

// Distribute 分发消息
// TODO: 增加日志记录
func (e *Engine) Distribute(frame *proto.TransferFrame, r *tcp.Remote) {
	defer framePool.Put(frame)

	var err error
	var needResp bool

	switch frame.Type {
	case proto.PMessageType: // 生产消息
		// 内部会就地修改 frame
		needResp, err = e.HandleProductionMessage(frame, r)

	case proto.RegisterMessageType: // 注册消费者
		// 内部会就地修改 frame
		needResp, err = e.HandleRegisterMessage(frame, r)
	}

	// 错误，或不需要回写返回值
	if err != nil || !needResp {
		return
	}

	// 重新构建并写入消息帧
	_bytes, err := frame.Build()
	if err != nil {
		return
	}
	_, err = r.Write(_bytes)
	err = r.Drain()
	if err != nil {
		return
	}
}

// HandleRegisterMessage 处理注册消息
func (e *Engine) HandleRegisterMessage(frame *proto.TransferFrame, r *tcp.Remote) (bool, error) {
	msg, err := frame.ParseTo()
	rgm, ok := msg.(*proto.RegisterMessage)
	if err != nil || !ok {
		return false, fmt.Errorf("register message parse failed, %v", err)
	}

	switch rgm.Type {

	case proto.ProducerLinkType: // 注册生产者
		prod := &Producer{
			Conf: &ProducerConfig{Ack: rgm.Ack, TimerInterval: e.ProducerInterval()},
			Addr: r.Addr(),
			Conn: r,
		}
		e.producers[r.Index()] = prod // 记录生产者, 用于判断其后是否要返回消息投递后的确认消息

	case proto.ConsumerLinkType: // 注册消费者
		cons := &Consumer{
			Conf: &ConsumerConfig{Topics: rgm.Topics, Ack: rgm.Ack},
			Addr: r.Addr(),
			Conn: r,
		}

		e.consumers[r.Index()] = cons

		for _, name := range rgm.Topics {
			e.GetTopic([]byte(name)).AddConsumer(cons)
		}
	}

	// 无论如何都需要构建返回值
	resp := &proto.MessageResponse{
		Result:        true,
		Offset:        0,
		ReceiveTime:   time.Now(),
		TimerInterval: e.ProducerInterval(),
	}
	frame.Type = proto.RegisterMessageRespType
	frame.Data, err = resp.Build()
	if err != nil {
		return false, fmt.Errorf("register response message build failed: %v", err)
	}

	return true, nil
}

// HandleProductionMessage 处理生产者消息帧，此处需要判断生产者是否已注册
func (e *Engine) HandleProductionMessage(frame *proto.TransferFrame, r *tcp.Remote) (bool, error) {
	if !e.IsProducerRegister(r.Addr()) {
		return false, ErrProducerNotRegister
	}

	// 会存在多个消息封装为一个帧
	pms := make([]*proto.PMessage, 0)
	stream := bytes.NewReader(frame.Data)

	// 循环解析生产者消息
	var err error
	for err == nil && stream.Len() > 0 {
		pm := mp.GetPM()
		err = pm.ParseFrom(stream)
		if err == nil {
			pms = append(pms, pm)
		}
	}

	if len(pms) < 1 {
		return false, errors.New("message not found in frame")
	}

	// 若是批量发送数据,则取最后一条消息的偏移量
	var offset uint64 = 0
	for _, pm := range pms {
		offset = e.Publisher(pm)
	}

	consumer := e.consumers[r.Index()]
	if consumer != nil && consumer.NeedConfirm() {
		// 需要返回确认消息给客户端
		resp := &proto.MessageResponse{
			Result:      true,
			Offset:      offset,
			ReceiveTime: time.Now(),
		}
		_bytes, err2 := resp.Build()
		if err2 != nil {
			return false, fmt.Errorf("response build failed: %v", err2)
		}
		frame.Type = proto.MessageRespType
		frame.Data = _bytes
		return true, nil
	} else {
		// 不需要返回值
		return false, nil
	}
}

func (e *Engine) Run() {
	go func() {
		err := e.transfer.Start()
		if err != nil {
			e.conf.Logger.Error("server starts failed, ", err)
			os.Exit(1)
		}
	}()

	// 关闭开关, buffered
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit // 阻塞进程，直到接收到停止信号,准备关闭程序

	e.transfer.Stop()
}

func New(c ...Config) *Engine {
	var d Config

	if len(c) > 0 {
		d = c[0]
	} else {
		d = Config{
			Host:        "127.0.0.1",
			Port:        "9999",
			MaxOpenConn: 50,
			BufferSize:  200,
			Logger:      logger.NewDefaultLogger(),
		}
	}

	if d.BufferSize == 0 {
		d.BufferSize = 100
	}
	if d.Logger == nil {
		d.Logger = logger.NewDefaultLogger()
	}

	if !(d.MaxOpenConn > 0 && d.MaxOpenConn <= 100) {
		d.MaxOpenConn = 50
	}

	eng := &Engine{
		conf: &Config{
			Host:        d.Host,
			Port:        d.Port,
			MaxOpenConn: d.MaxOpenConn,
			BufferSize:  d.BufferSize,
			Logger:      d.Logger,
		},
		producers: make([]*Producer, d.MaxOpenConn),
		consumers: make([]*Consumer, d.MaxOpenConn),
		topics:    &sync.Map{},
	}

	eng.transfer = &Transfer{
		logger: d.Logger,
		mq:     eng,
	}
	eng.transfer.SetEngine(eng).init()

	return eng
}
