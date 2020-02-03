package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/RedHatInsights/platform-receptor-controller/internal/controller"
	"github.com/RedHatInsights/platform-receptor-controller/internal/platform/queue"
	"github.com/RedHatInsights/platform-receptor-controller/internal/receptor/protocol"
	"github.com/gorilla/websocket"
	"github.com/segmentio/kafka-go"
)

type rcClient struct {
	account string

	node_id string

	// socket is the web socket for this client.
	socket *websocket.Conn

	// send is a channel on which messages are sent.
	send chan controller.Work

	cancel context.CancelFunc

	config *WebSocketConfig

	writer *kafka.Writer
}

func (c *rcClient) SendWork(w controller.Work) {
	c.send <- w
}

func (c *rcClient) DisconnectReceptorNetwork() {
	log.Println("DisconnectReceptorNetwork()")
	c.socket.Close()
}

func (c *rcClient) Close() {
	// FIXME:  Think through this a bit more.  On close, we might need to to try
	// send a CloseMessage to the client??
	c.cancel()
}

func (c *rcClient) read(ctx context.Context) {
	defer func() {
		c.socket.Close()
		log.Println("WebSocket reader leaving!")
	}()

	go c.write(ctx)
	// go c.consume(ctx)

	c.configurePongHandler()

	for {
		log.Println("WebSocket reader waiting for message...")
		messageType, r, err := c.socket.NextReader()
		log.Println("Websocket reader: got message")
		log.Println("messageType:", messageType)

		if err != nil {
			log.Println("WebSocket reader got a error: ", err)
			return
		}

		message, err := protocol.ReadMessage(r)
		if err != nil {
			log.Println("WebSocket reader got a error: ", err)
			return
		}

		log.Printf("Websocket reader message: %+v\n", message)
		log.Println("Websocket reader message type:", message.Type())

		c.produce(ctx, message)
	}
}

func (c *rcClient) configurePongHandler() {

	if c.config.PongWait > 0 {
		log.Println("Configuring a pong handler with a deadline of ", c.config.PongWait)
		c.socket.SetReadDeadline(time.Now().Add(c.config.PongWait))

		c.socket.SetPongHandler(func(data string) error {
			log.Println("WebSocket reader - got a pong")
			c.socket.SetReadDeadline(time.Now().Add(c.config.PongWait))
			return nil
		})
	} else {
		log.Println("Pong handler has been disabled")
	}
}

func (c *rcClient) write(ctx context.Context) {

	pingTicker := c.configurePingTicker()

	defer func() {
		c.socket.Close()
		pingTicker.Stop()
		log.Println("WebSocket writer leaving!")
	}()

	for {
		log.Println("WebSocket writer - Waiting for something to send")

		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			log.Println("Websocket writer needs to send msg:", msg)

			payloadMessage, messageID, err := protocol.BuildPayloadMessage(
				c.config.ReceptorControllerNodeId,
				msg.Recipient,
				msg.RouteList,
				"directive",
				msg.Directive,
				msg.Payload)
			log.Printf("Sending PayloadMessage - %s\n", *messageID)

			c.socket.SetWriteDeadline(time.Now().Add(c.config.WriteWait))
			w, err := c.socket.NextWriter(websocket.BinaryMessage)
			if err != nil {
				log.Println("WebSocket writer - error!  Closing connection!")
				return
			}

			err = protocol.WriteMessage(w, payloadMessage)
			if err != nil {
				log.Println("WebSocket writer - error writing the message!  Closing connection!")
				return
			}
			w.Close()
		case <-pingTicker.C:
			log.Println("WebSocket writer - sending PingMessage")
			c.socket.SetWriteDeadline(time.Now().Add(c.config.WriteWait))
			if err := c.socket.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Println("WebSocket writer - error sending ping message!  Closing connection!")
				return
			}
		}
	}
}

func (c *rcClient) configurePingTicker() *time.Ticker {

	if c.config.PingPeriod > 0 {
		log.Println("Configuring a ping to fire every ", c.config.PingPeriod)
		return time.NewTicker(c.config.PingPeriod)
	} else {
		log.Println("Pings are disabled")
		// To disable sending ping messages, we create a ticker that doesn't ever fire
		ticker := time.NewTicker(40 * 60 * time.Minute)
		ticker.Stop()
		return ticker
	}
}

func (c *rcClient) consume(ctx context.Context) {
	r := queue.StartConsumer(queue.GetConsumer())

	defer func() {
		err := r.Close()
		if err != nil {
			log.Println("Kafka job reader - error closing consumer: ", err)
			return
		}
		log.Println("Kafka job reader leaving...")
	}()

	for {
		log.Printf("Kafka job reader - waiting on a message from kafka...")
		m, err := r.ReadMessage(ctx)
		if err != nil {
			// FIXME:  do we need to call cancel here??
			log.Println("Kafka job reader - error reading message: ", err)
			break
		}

		log.Printf("Kafka job reader - received message from %s-%d [%d]: %s: %s\n",
			m.Topic,
			m.Partition,
			m.Offset,
			string(m.Key),
			string(m.Value))

		if string(m.Key) == c.account {
			// FIXME:
			w := controller.Work{}
			c.SendWork(w)
		} else {
			log.Println("Kafka job reader - received message but did not send. Account number not found.")
		}
	}
}

func (c *rcClient) produce(ctx context.Context, m protocol.Message) error {
	type ResponseMessage struct {
		Account   string      `json:"account"`
		Sender    string      `json:"sender"`
		MessageID string      `json:"message_id"`
		Payload   interface{} `json:"payload"`
	}

	if m.Type() != protocol.PayloadMessageType {
		log.Printf("Unable to dispatch message (type: %d): %s", m.Type(), m)
		return nil
	}

	payloadMessage, ok := m.(*protocol.PayloadMessage)
	if !ok {
		log.Println("Unable to convert message into PayloadMessage")
		return nil
	}

	// verify this message was meant for this receptor/peer (probably want a uuid here)
	if payloadMessage.RoutingInfo.Recipient != c.config.ReceptorControllerNodeId {
		log.Println("Recieved message that was not intended for this node")
		return nil
	}

	messageId := payloadMessage.Data.InResponseTo

	responseMessage := ResponseMessage{
		Account:   c.account,
		Sender:    payloadMessage.RoutingInfo.Sender,
		MessageID: messageId,
		Payload:   payloadMessage.Data.RawPayload,
	}

	log.Println("Dispatching response:", responseMessage)

	jsonResponseMessage, err := json.Marshal(responseMessage)
	if err != nil {
		log.Println("JSON marshal of ResponseMessage failed, err:", err)
		return nil
	}

	c.writer.WriteMessages(ctx,
		kafka.Message{
			Key:   []byte(messageId),
			Value: jsonResponseMessage,
		})

	return nil
}

func (c *rcClient) performHandshake() (string, error) {

	c.socket.SetReadDeadline(time.Now().Add(c.config.HandshakeReadWait))
	defer c.socket.SetReadDeadline(time.Time{})

	messageType, r, err := c.socket.NextReader()
	log.Println("WebSocket reader got a message...")
	if err != nil {
		log.Println("WebSocket reader - error: ", err)
		return "", err
	}

	if messageType != websocket.BinaryMessage {
		log.Printf("WebSocket reader: invalid type, expected %d, got %d", websocket.BinaryMessage, messageType)
		return "", errors.New("websocket reader: invalid message type")
	}

	message, err := protocol.ReadMessage(r)
	if err != nil {
		log.Println("Websocket reader - error reading/parsing message: ", err)
		return "", err
	}
	log.Println("Websocket reader message:", message)
	log.Println("Websocket reader message type:", message.Type())

	if message.Type() != protocol.HiMessageType {
		log.Printf("WebSocket reader: invalid type, expected %d, got %d", protocol.HiMessageType, message.Type())
		return "", errors.New("websocket reader: invalid receptor message type")
	}

	hiMessage, ok := message.(*protocol.HiMessage)
	if ok != true {
		log.Println("Websocket reader - error casting message to HiMessage")
		return "", errors.New("websocket reader: invalid receptor message type")
	}

	log.Printf("Received a hi message from receptor node %s\n", hiMessage.ID)

	log.Println("WebSocket writer - sending HI")

	c.socket.SetWriteDeadline(time.Now().Add(c.config.WriteWait))
	w, err := c.socket.NextWriter(websocket.BinaryMessage)
	if err != nil {
		log.Println("WebSocket writer - error getting next writer: ", err)
		return "", err
	}

	defer w.Close()

	responseHiMessage := protocol.HiMessage{Command: "HI", ID: c.config.ReceptorControllerNodeId}

	err = protocol.WriteMessage(w, &responseHiMessage)
	if err != nil {
		log.Println("WebSocket writer - error writing message: ", err)
		return "", err
	}

	log.Println("WebSocket writer - sent HI")

	return hiMessage.ID, nil
}