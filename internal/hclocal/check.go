package hclocal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"hcgateway/internal/tlspsk"
)

// ErrWrongKey - по этому адресу прибор Home Connect, но он не принял ключ: это другой прибор или
// ключи устарели.
var ErrWrongKey = errors.New("прибор не принял ключ")

// Check проверяет связь с прибором: подключается к host ключами из каталога dir и ждёт первого
// сообщения прибора. Состояние прибора не меняется.
func Check(ctx context.Context, dir, haID, host string) error {
	_, k, _, err := load(dir, haID)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", errNoKeys, err)
	}
	if err != nil {
		return err
	}
	p, err := Dial(ctx, host, k.mode, k.psk, k.iv, identity)
	if err != nil {
		if tlspsk.WrongKey(err) {
			return fmt.Errorf("%w: %w", ErrWrongKey, err)
		}
		return err
	}
	defer p.Close()
	_, err = p.Handshake(ctx)
	return err
}
