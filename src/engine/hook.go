package engine

import (
	"errors"
	"github.com/Chendemo12/micromq/src/proto"
	"github.com/Chendemo12/micromq/src/transfer"
)

// HookHandler 消息处理方法
type HookHandler func(frame *proto.TransferFrame, con transfer.Conn) error

type Hook struct {
	Type       proto.MessageType // 据此判断消息是否已实现
	ACKDefined bool              // 消息定义处,定义需要有返回值
	Handler    HookHandler
}

// ================================== 链式处理请求 ==================================

type ChainArgs struct {
	frame    *proto.TransferFrame
	con      transfer.Conn
	resp     *proto.MessageResponse
	producer *Producer
	rm       *proto.RegisterMessage
	pms      []*proto.PMessage
	stopErr  error // 不回复客户端的原因
}

func (args *ChainArgs) Reset() {
	args.frame = nil
	args.con = nil
	args.producer = nil
	args.rm = nil
	args.pms = nil
	args.resp = nil
	args.stopErr = nil
}

// ReplyClient 是否需要回复客户端
func (args *ChainArgs) ReplyClient() bool {
	if args.stopErr != nil && errors.Is(args.stopErr, ErrNoNeedToReply) {
		return false
	}

	return args.stopErr == nil
}

// SetStopFlag 设置终止原因
func (args *ChainArgs) SetStopFlag(errs ...error) {
	if len(errs) > 0 {
		args.stopErr = errs[0]
	} else {
		args.stopErr = ErrNoNeedToReply
	}
}

// NoReplyReason 不回复客户端的原因
func (args *ChainArgs) NoReplyReason() error {
	return args.stopErr
}

type FlowHandler func(args *ChainArgs) (stop bool)
