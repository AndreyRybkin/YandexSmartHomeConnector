package hcapi

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Лимиты облака на клиента и аккаунт. Шлюз держит запросы ниже них сам: превышение
// блокирует клиента на минуту, а ошибки подряд - на 10 минут.
const (
	perSecond = 10
	burst     = 20
	perMinute = 50
	// actionsPerMinute - запусков и отдельно остановок программы в минуту.
	actionsPerMinute = 5
)

// limiter - очередь запросов: не больше 10 в секунду со всплеском до 20, не больше 50 в минуту
// и пауза, которую облако задало заголовком Retry-After.
type limiter struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu sync.Mutex
	// tokens и last - ведро на 10 запросов в секунду.
	tokens float64
	last   time.Time
	// minute - время запросов за последнюю минуту.
	minute []time.Time
	// blockedUntil - до какого времени облако просило не присылать запросов.
	blockedUntil time.Time
	// actions - запуски и остановки программ за последнюю минуту.
	actions map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{now: time.Now, sleep: sleepCtx, tokens: burst, actions: map[string][]time.Time{}}
}

// wait ждёт очереди запроса. Если ждать дольше, чем позволяет ctx, сразу возвращает ErrBusy:
// команда Алисы не ждёт минуту.
func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := l.now()
		d := l.delayLocked(now)
		if d <= 0 {
			l.takeLocked(now)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		if deadline, ok := ctx.Deadline(); ok && now.Add(d).After(deadline) {
			return ErrBusy
		}
		if err := l.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// delayLocked - сколько ждать до следующего запроса.
func (l *limiter) delayLocked(now time.Time) time.Duration {
	l.refillLocked(now)
	l.minute = slices.DeleteFunc(l.minute, func(t time.Time) bool { return now.Sub(t) >= time.Minute })
	var d time.Duration
	if wait := l.blockedUntil.Sub(now); wait > d {
		d = wait
	}
	if len(l.minute) >= perMinute {
		if wait := l.minute[0].Add(time.Minute).Sub(now); wait > d {
			d = wait
		}
	}
	if l.tokens < 1 {
		if wait := time.Duration((1 - l.tokens) / perSecond * float64(time.Second)); wait > d {
			d = wait
		}
	}
	return d
}

func (l *limiter) refillLocked(now time.Time) {
	if !l.last.IsZero() {
		l.tokens = min(burst, l.tokens+now.Sub(l.last).Seconds()*perSecond)
	}
	l.last = now
}

func (l *limiter) takeLocked(now time.Time) {
	l.tokens--
	l.minute = append(l.minute, now)
}

// block - облако ответило Retry-After: запросов не будет до now+d.
func (l *limiter) block(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.blockedUntil) {
		l.blockedUntil = until
	}
}

// action считает запуск или остановку программы. false - лимит минуты исчерпан, команду не отправлять.
func (l *limiter) action(kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	list := slices.DeleteFunc(l.actions[kind], func(t time.Time) bool { return now.Sub(t) >= time.Minute })
	if len(list) >= actionsPerMinute {
		l.actions[kind] = list
		return false
	}
	l.actions[kind] = append(list, now)
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
