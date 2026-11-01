package webos

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// message - сообщение SSAP: запрос, ответ, подписка или регистрация. Ответ телевизор
// присылает с id запроса, а изменения по подписке - с id подписки.
type message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	URI     string          `json:"uri,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// errClosed - соединение с телевизором оборвалось.
var errClosed = errors.New("соединение с телевизором оборвалось")

// session - соединение с телевизором: читает сообщения и раздаёт их ожидающим ответа и
// подпискам.
type session struct {
	ws   *websocket.Conn
	done chan struct{}
	// err - почему чтение закончилось; читать только после done.
	err error

	// wmu - запись по одному сообщению.
	wmu sync.Mutex

	mu      sync.Mutex
	next    int
	waiting map[string]chan message
	subs    map[string]func(json.RawMessage)
}

func newSession(ws *websocket.Conn) *session {
	return &session{ws: ws, done: make(chan struct{}), waiting: map[string]chan message{},
		subs: map[string]func(json.RawMessage){}}
}

// read читает сообщения, пока соединение не закроется.
func (s *session) read() {
	defer close(s.done)
	_ = s.ws.SetReadDeadline(time.Now().Add(readIdle))
	s.ws.SetPongHandler(func(string) error { return s.ws.SetReadDeadline(time.Now().Add(readIdle)) })
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			s.err = err
			return
		}
		_ = s.ws.SetReadDeadline(time.Now().Add(readIdle))
		var m message
		if json.Unmarshal(data, &m) != nil || m.ID == "" {
			continue
		}
		s.mu.Lock()
		ch, sub := s.waiting[m.ID], s.subs[m.ID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
		if sub != nil && m.Type == "response" {
			sub(m.Payload)
		}
	}
}

// send отправляет сообщение.
func (s *session) send(v any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(requestTimeout))
	return s.ws.WriteJSON(v)
}

func (s *session) ping() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(requestTimeout))
}

func (s *session) nextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return "req_" + strconv.Itoa(s.next)
}

// start отправляет сообщение и возвращает канал ответов с его id: на регистрацию телевизор
// отвечает дважды.
func (s *session) start(typ, id, uri string, payload any) (chan message, error) {
	ch := make(chan message, 4)
	s.mu.Lock()
	s.waiting[id] = ch
	s.mu.Unlock()
	m := map[string]any{"type": typ, "id": id}
	if uri != "" {
		m["uri"] = uri
	}
	if payload != nil {
		m["payload"] = payload
	}
	if err := s.send(m); err != nil {
		s.stop(id)
		return nil, err
	}
	return ch, nil
}

func (s *session) stop(id string) {
	s.mu.Lock()
	delete(s.waiting, id)
	s.mu.Unlock()
}

// call отправляет запрос и ждёт ответа. Ответ с ошибкой или с returnValue false - отказ
// телевизора.
func (s *session) call(ctx context.Context, typ, id, uri string, payload any, timeout time.Duration) (json.RawMessage, error) {
	ch, err := s.start(typ, id, uri, payload)
	if err != nil {
		return nil, err
	}
	defer s.stop(id)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case m := <-ch:
		if m.Type == "error" {
			return nil, &errTV{text: m.Error}
		}
		var rv struct {
			ReturnValue *bool  `json:"returnValue"`
			ErrorText   string `json:"errorText"`
		}
		if json.Unmarshal(m.Payload, &rv) == nil && rv.ReturnValue != nil && !*rv.ReturnValue {
			return nil, &errTV{text: rv.ErrorText}
		}
		return m.Payload, nil
	case <-s.done:
		return nil, errClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("телевизор не ответил")
	}
}

// subscribe подписывается на uri: handle получает первый ответ и каждое изменение. Подписку,
// на которую телевизор ответил ошибкой, шлюз снимает.
func (s *session) subscribe(ctx context.Context, id, uri string, handle func(json.RawMessage)) error {
	s.mu.Lock()
	s.subs[id] = handle
	s.mu.Unlock()
	ch, err := s.start("subscribe", id, uri, nil)
	if err == nil {
		defer s.stop(id)
		timer := time.NewTimer(requestTimeout)
		defer timer.Stop()
		select {
		case m := <-ch:
			if m.Type == "error" {
				err = &errTV{text: m.Error}
			}
		case <-s.done:
			err = errClosed
		case <-ctx.Done():
			err = ctx.Err()
		case <-timer.C:
			err = errors.New("телевизор не ответил на подписку")
		}
	}
	if err != nil {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
	return err
}
