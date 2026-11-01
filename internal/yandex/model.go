// Package yandex - провайдер умного дома Яндекса: запросы платформы и уведомления о состоянии.
// Приборы провайдер видит только как устройства Алисы (пакет alice).
package yandex

import "hcgateway/internal/alice"

// Запросы платформы и ответы на команды. Типы устройств - в пакете alice.

type queryRequest struct {
	Devices []struct {
		ID string `json:"id"`
	} `json:"devices"`
}

type actionRequest struct {
	Payload struct {
		Devices []actionDevice `json:"devices"`
	} `json:"payload"`
}

type actionDevice struct {
	ID           string         `json:"id"`
	Capabilities []alice.Action `json:"capabilities"`
}

type actionDeviceResult struct {
	ID           string           `json:"id"`
	Capabilities []alice.CapState `json:"capabilities,omitempty"`
}

type resultState struct {
	Instance     string       `json:"instance"`
	ActionResult actionResult `json:"action_result"`
}

type actionResult struct {
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}
