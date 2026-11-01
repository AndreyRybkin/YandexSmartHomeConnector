package hcapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hcgateway/internal/metrics"
)

// maxEventLine - предел строки потока событий.
const maxEventLine = 1 << 20

// streamIdle - если из потока столько времени не приходит ни строки, даже KEEP-ALIVE,
// поток считается оборванным. Интервал KEEP-ALIVE документация облака не называет. Тесты его сокращают.
var streamIdle = 3 * time.Minute

// Типы событий потока.
const (
	EventKeepAlive    = "KEEP-ALIVE"
	EventStatus       = "STATUS"
	EventEvent        = "EVENT"
	EventNotify       = "NOTIFY"
	EventConnected    = "CONNECTED"
	EventDisconnected = "DISCONNECTED"
	EventPaired       = "PAIRED"
	EventDepaired     = "DEPAIRED"
)

// ErrStreamIdle - из потока событий долго ничего не приходило.
var ErrStreamIdle = errors.New("поток событий Home Connect молчит")

// Event - событие потока: тип, прибор и изменения. haId облако передаёт в поле id.
type Event struct {
	Type  string
	HaID  string
	Items []Value
}

// Events открывает поток событий всех приборов аккаунта и вызывает handle на каждое событие,
// пока поток не оборвётся или ctx не отменён. connected вызывается, когда поток открыт.
// Открытие потока - запрос из лимитов облака.
func (c *Client) Events(ctx context.Context, connected func(), handle func(Event)) error {
	token, err := c.access(ctx)
	if err != nil {
		return err
	}
	if err := c.lim.wait(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config().BaseURL+"/api/homeappliances/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		metrics.SourceRequest(SourceName, "error")
		c.result(false)
		return err
	}
	defer resp.Body.Close()
	metrics.SourceRequest(SourceName, strconv.Itoa(resp.StatusCode))
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			c.retryAfter(resp.Header.Get("Retry-After"))
		case http.StatusUnauthorized:
			c.mu.Lock()
			t := c.tokens
			c.mu.Unlock()
			if t != nil {
				c.refresh(ctx, t)
			}
		}
		c.result(false)
		return parseError(resp.StatusCode, data)
	}
	c.result(true)
	connected()

	idle := time.AfterFunc(streamIdle, func() { cancel(ErrStreamIdle) })
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	var (
		ev   Event
		data []string
	)
	for sc.Scan() {
		idle.Reset(streamIdle)
		line := sc.Text()
		switch {
		case line == "":
			if ev.Type != "" || len(data) > 0 {
				ev.Items = parseItems(strings.Join(data, "\n"))
				handle(ev)
			}
			ev, data = Event{}, nil
		case strings.HasPrefix(line, ":"):
			// Комментарий: облако так держит соединение.
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Type = value
			case "data":
				data = append(data, value)
			case "id":
				ev.HaID = value
			}
		}
	}
	if errors.Is(context.Cause(ctx), ErrStreamIdle) {
		return ErrStreamIdle
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// parseItems разбирает данные события: список items или одно изменение.
func parseItems(data string) []Value {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil
	}
	var list struct {
		Items []Value `json:"items"`
	}
	if err := json.Unmarshal([]byte(data), &list); err == nil && list.Items != nil {
		return list.Items
	}
	var one Value
	if err := json.Unmarshal([]byte(data), &one); err == nil && one.Key != "" {
		return []Value{one}
	}
	return nil
}
