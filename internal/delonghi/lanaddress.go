package delonghi

import (
	"context"
	"net/netip"
)

// LanAddress - IP машины в домашней сети: машина сама сообщает его облаку в тени
// MachineStatus. Пусто - машина его не сообщила или прислала не IPv4.
func (a *Account) LanAddress(ctx context.Context, machineName string) (string, error) {
	r, err := a.readShadow(ctx, machineName, "MachineStatus")
	if err != nil {
		return "", err
	}
	raw, _ := r["LanIpAddress"].(string)
	ip, err := netip.ParseAddr(raw)
	if err != nil || !ip.Is4() {
		return "", nil
	}
	return ip.String(), nil
}
