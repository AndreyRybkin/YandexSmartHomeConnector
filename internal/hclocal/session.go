package hclocal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Действия сообщений.
const (
	actionGet      = "GET"
	actionPost     = "POST"
	actionNotify   = "NOTIFY"
	actionResponse = "RESPONSE"
)

// errClosed - шлюз закрыл соединение сам.
var errClosed = errors.New("соединение закрыто")

// message - сообщение прибора.
type message struct {
	SID      int64             `json:"sID"`
	MsgID    int64             `json:"msgID"`
	Resource string            `json:"resource"`
	Version  int               `json:"version"`
	Action   string            `json:"action"`
	Data     []json.RawMessage `json:"data"`
	// Code - код ошибки: есть только в ответе, который запрос отклоняет.
	Code json.RawMessage `json:"code"`
}

// outMessage - сообщение шлюза. Данные - всегда список, как у приложения Home Connect.
type outMessage struct {
	SID      int64  `json:"sID"`
	MsgID    int64  `json:"msgID"`
	Resource string `json:"resource"`
	Version  int    `json:"version"`
	Action   string `json:"action"`
	Data     []any  `json:"data,omitempty"`
}

// codeError - прибор отклонил запрос кодом ошибки.
type codeError struct {
	resource, code string
}

func (e *codeError) Error() string {
	return fmt.Sprintf("прибор отклонил %s: код %s", e.resource, e.code)
}

// session - протокол поверх одного соединения: номера сообщений, версии служб и ожидающие ответа запросы.
type session struct {
	link *link
	// initial - первое сообщение прибора, /ei/initialValues.
	initial chan *message
	closed  chan struct{}
	once    sync.Once
	err     error

	// send - отправка: номер сообщения назначается и сообщение уходит по порядку.
	send sync.Mutex

	mu sync.Mutex
	// sid - сессия, которую назначил прибор; next - номер следующего запроса шлюза.
	sid, next int64
	services  map[string]int
	pending   map[int64]chan *message
}

func newSession(l *link) *session {
	return &session{link: l, initial: make(chan *message, 1), closed: make(chan struct{}),
		services: map[string]int{}, pending: map[int64]chan *message{}}
}

// fail закрывает соединение с причиной err: ожидающие запросы получают ошибку.
func (s *session) fail(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.closed)
		s.link.ws.Close()
	})
}

func (s *session) error() error {
	<-s.closed
	return s.err
}

// begin запоминает сессию и первый номер сообщения, которые назначил прибор.
func (s *session) begin(sid, first int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sid, s.next = sid, first
}

func (s *session) setServices(data []json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range data {
		var sv struct {
			Service string `json:"service"`
			Version int    `json:"version"`
		}
		if json.Unmarshal(d, &sv) == nil && sv.Service != "" {
			s.services[sv.Service] = sv.Version
		}
	}
}

func (s *session) has(service string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.services[service]
	return ok
}

// write отправляет запрос: version - версия по умолчанию, служба из /ci/services её заменяет.
// wait получает ответ, если не nil.
func (s *session) write(action, resource string, version int, data any, wait chan *message) (int64, error) {
	s.send.Lock()
	defer s.send.Unlock()
	s.mu.Lock()
	id := s.next
	s.next++
	if service, _, ok := strings.Cut(strings.TrimPrefix(resource, "/"), "/"); ok {
		if v, ok := s.services[service]; ok {
			version = v
		}
	}
	if wait != nil {
		s.pending[id] = wait
	}
	m := outMessage{SID: s.sid, MsgID: id, Resource: resource, Version: version, Action: action}
	s.mu.Unlock()
	if data != nil {
		m.Data = []any{data}
	}
	b, err := json.Marshal(m)
	if err == nil {
		err = s.link.send(b)
	}
	return id, err
}

// post отправляет запрос, не дожидаясь ответа.
func (s *session) post(action, resource string, version int, data any) error {
	_, err := s.write(action, resource, version, data, nil)
	return err
}

// call отправляет запрос и ждёт ответа не дольше requestTimeout.
func (s *session) call(ctx context.Context, action, resource string, version int, data any) (*message, error) {
	wait := make(chan *message, 1)
	id, err := s.write(action, resource, version, data, wait)
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(requestTimeout)
	defer t.Stop()
	select {
	case m := <-wait:
		if len(m.Code) > 0 && string(m.Code) != "null" {
			return m, &codeError{resource: resource, code: string(m.Code)}
		}
		return m, nil
	case <-t.C:
		return nil, fmt.Errorf("прибор не ответил на %s за %v", resource, requestTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, s.err
	}
}

// deliver отдаёт ответ запросу, который его ждёт.
func (s *session) deliver(m *message) bool {
	s.mu.Lock()
	wait := s.pending[m.MsgID]
	delete(s.pending, m.MsgID)
	s.mu.Unlock()
	if wait == nil {
		return false
	}
	wait <- m
	return true
}

// reply отвечает на запрос прибора теми же сессией, номером, адресом и версией.
func (s *session) reply(m *message, data any) error {
	b, err := json.Marshal(outMessage{SID: m.SID, MsgID: m.MsgID, Resource: m.Resource, Version: m.Version,
		Action: actionResponse, Data: []any{data}})
	if err != nil {
		return err
	}
	s.send.Lock()
	defer s.send.Unlock()
	return s.link.send(b)
}
