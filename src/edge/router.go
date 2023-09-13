package edge

import (
	"github.com/Chendemo12/fastapi"
	"github.com/Chendemo12/fastapi-tool/helper"
	"github.com/Chendemo12/micromq/src/engine"
	"github.com/Chendemo12/micromq/src/proto"
	"time"
)

var Broker *engine.Engine
var Crypto proto.Crypto = &proto.NoCrypto{}

func NewApp(conf *fastapi.Config, broker *engine.Engine) *fastapi.FastApi {
	if broker != nil {
		Broker = broker
		Crypto = broker.Crypto()
	}

	conf.DisableResponseValidate = true
	conf.EnableMultipleProcess = false

	return fastapi.New(*conf)
}

// Router edge路由组
func Router() *fastapi.Router {
	var router = fastapi.APIRouter("/api/edge", []string{"EDGE"})

	{
		router.Post("/product", PostProducerMessage, fastapi.Option{
			Summary:       "发送一个生产者消息",
			Description:   "阻塞式发送生产者消息，此接口会在消息成功发送给消费者后返回",
			RequestModel:  &ProducerForm{},
			ResponseModel: &ProductResponse{},
		})

		router.Post("/product/async", AsyncPostProducerMessage, fastapi.Option{
			Summary:       "异步发送一个生产者消息",
			Description:   "非阻塞式发送生产者消息，服务端会在消息解析成功后立刻返回结果，不保证消息已发送给消费者",
			RequestModel:  &ProducerForm{},
			ResponseModel: &ProductResponse{},
		})
	}

	return router
}

type ProducerForm struct {
	fastapi.BaseModel
	Token string `json:"token,omitempty" description:"认证密钥"`
	Topic string `json:"topic" description:"消息主题"`
	Key   string `json:"key" description:"消息键"`
	Value string `json:"value" description:"base64编码后的消息体"`
}

func (m *ProducerForm) SchemaDesc() string {
	return `生产者消息投递表单, 不允许将多个消息编码成一个消息帧; 
token若为空则认为不加密; 
value是对加密后的消息体进行base64编码后的结果,依据token判断是否需要解密`
}

func (m *ProducerForm) IsEncrypt() bool { return m.Topic != "" }

type ProductResponse struct {
	fastapi.BaseModel
	// 仅当 Accepted 时才认为服务器接受了请求并下方了有效的参数
	Status       string `json:"status" validate:"oneof=Accepted UnmarshalFailed TokenIncorrect Let-ReRegister Refused" description:"消息接收状态"`
	Offset       uint64 `json:"offset" description:"消息偏移量"`
	ResponseTime int64  `json:"response_time" description:"服务端返回消息时的时间戳"`
	Message      string `json:"message" description:"额外的消息描述"`
}

func (m *ProductResponse) SchemaDesc() string {
	return "消息返回值; 仅当 status=Accepted 时才认为服务器接受了请求并正确的处理了消息"
}

func toPMessage(c *fastapi.Context) (*proto.PMessage, *fastapi.Response) {
	form := &ProducerForm{}
	resp := c.ShouldBindJSON(form)
	if resp != nil {
		return nil, resp
	}

	pm := &proto.PMessage{}
	// 首先反序列化消息体
	decode, err := helper.Base64Decode(form.Value)
	if err != nil {
		return nil, c.OKResponse(&ProductResponse{
			Status:       "UnmarshalFailed",
			Offset:       0,
			ResponseTime: time.Now().Unix(),
			Message:      err.Error(),
		})
	}

	// 解密消息
	if !form.IsEncrypt() {
		pm.Value = decode
	} else {
		if !Broker.IsTokenCorrect(form.Token) {
			return nil, c.OKResponse(&ProductResponse{
				Status:       proto.GetMessageResponseStatusText(proto.TokenIncorrectStatus),
				Offset:       0,
				ResponseTime: time.Now().Unix(),
				Message:      err.Error(),
			})
		} else {
			_bytes, _err := Crypto.Decrypt(decode)
			if _err != nil {
				return nil, c.OKResponse(&ProductResponse{
					Status:       proto.GetMessageResponseStatusText(proto.TokenIncorrectStatus),
					Offset:       0,
					ResponseTime: time.Now().Unix(),
					Message:      _err.Error(),
				})
			}
			pm.Value = _bytes
		}
	}

	pm.Key = helper.S2B(form.Key)
	pm.Topic = helper.S2B(form.Token)

	return pm, nil
}

// PostProducerMessage 同步发送消息
func PostProducerMessage(c *fastapi.Context) *fastapi.Response {
	pm, resp := toPMessage(c)
	if resp != nil {
		return resp
	}
	respForm := &ProductResponse{}
	respForm.Status = proto.GetMessageResponseStatusText(proto.AcceptedStatus)
	respForm.Offset = Broker.Publisher(pm)
	respForm.ResponseTime = time.Now().Unix()

	return c.OKResponse(respForm)
}

// AsyncPostProducerMessage 异步生产消息
func AsyncPostProducerMessage(c *fastapi.Context) *fastapi.Response {
	pm, resp := toPMessage(c)
	if resp != nil {
		return resp
	}
	respForm := &ProductResponse{}
	respForm.Offset = Broker.Publisher(pm)
	respForm.Status = proto.GetMessageResponseStatusText(proto.AcceptedStatus)
	respForm.ResponseTime = time.Now().Unix()

	return c.OKResponse(respForm)
}
