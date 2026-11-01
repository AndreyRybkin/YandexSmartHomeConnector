package samsungtv

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// upnpDevice - устройство из описания UPnP: службы и вложенные устройства.
type upnpDevice struct {
	Services []struct {
		Type    string `xml:"serviceType"`
		Control string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

func (d *upnpDevice) control(service string) string {
	for _, s := range d.Services {
		if s.Type == service {
			return s.Control
		}
	}
	for i := range d.Devices {
		if c := d.Devices[i].control(service); c != "" {
			return c
		}
	}
	return ""
}

// renderingControl - адрес службы громкости из описания UPnP телевизора. Адрес шлюз
// запоминает: он не меняется, пока телевизор тот же.
func (t *TV) renderingControl(ctx context.Context) (string, error) {
	t.mu.Lock()
	ctl := t.control
	t.mu.Unlock()
	if ctl != "" {
		return ctl, nil
	}
	body, err := t.get(ctx, t.upnpURL)
	if err != nil {
		return "", err
	}
	var root struct {
		Device upnpDevice `xml:"device"`
	}
	if err := xml.Unmarshal(body, &root); err != nil {
		return "", fmt.Errorf("описание UPnP не разобрано: %w", err)
	}
	rel := root.Device.control(renderingControl)
	if rel == "" {
		return "", errors.New("в описании UPnP нет службы громкости")
	}
	base, err := url.Parse(t.upnpURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(rel)
	if err != nil {
		return "", err
	}
	u := base.ResolveReference(ref)
	if u.Host != base.Host {
		// Служба на чужом адресе: запрос ушёл бы не телевизору.
		return "", errors.New("служба громкости UPnP не на адресе телевизора")
	}
	t.mu.Lock()
	t.control = u.String()
	t.mu.Unlock()
	return u.String(), nil
}

// soap выполняет действие службы громкости и возвращает текст элемента result. Аргументы
// идут после InstanceID и Channel.
func (t *TV) soap(ctx context.Context, control, action, result string, args map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` + action + ` xmlns:u="` + renderingControl +
		`"><InstanceID>0</InstanceID><Channel>Master</Channel>`)
	for k, v := range args {
		b.WriteString("<" + k + ">")
		xml.EscapeText(&b, []byte(v))
		b.WriteString("</" + k + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, control, strings.NewReader(b.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+renderingControl+"#"+action+`"`)
	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("UPnP %s: HTTP %d", action, resp.StatusCode)
	}
	if result == "" {
		return "", nil
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("UPnP %s: в ответе нет %s", action, result)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == result {
			var v string
			if err := dec.DecodeElement(&v, &se); err != nil {
				return "", err
			}
			return strings.TrimSpace(v), nil
		}
	}
}
