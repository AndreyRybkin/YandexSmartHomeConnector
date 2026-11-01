// Package hcdesc разбирает XML-описание прибора Home Connect.
// Прибор в локальной сети называет функции числовыми номерами (uid),
// а описание переводит их в имена вида BSH.Common.Status.DoorState и
// перечисляет допустимые значения.
package hcdesc

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Feature - функция прибора: состояние, настройка, событие, команда, параметр или программа.
type Feature struct {
	UID uint32 `json:"uid"`
	// Name - имя из FeatureMapping.xml; пустое, если номера там нет.
	Name string `json:"name,omitempty"`
	// Kind - имя XML-элемента в DeviceDescription.xml, например status или program.
	Kind string `json:"kind"`
	// Attrs - остальные атрибуты элемента: access, available, min, max, refCID и другие.
	Attrs map[string]string `json:"attrs,omitempty"`
	// Values - значения перечисления: число -> имя.
	Values map[int]string `json:"values,omitempty"`
	// Enum - имя перечисления, например BSH.Common.EnumType.DoorState. Полное имя значения,
	// как его называет облако, - Enum, точка и имя из Values.
	Enum string `json:"enum,omitempty"`
}

// EnumValue - полное имя значения перечисления: BSH.Common.EnumType.DoorState.Open.
func (f *Feature) EnumValue(member string) string {
	if f.Enum == "" || strings.HasPrefix(member, f.Enum+".") {
		return member
	}
	return f.Enum + "." + member
}

// Description - функции прибора по номеру.
type Description struct {
	Features map[uint32]*Feature `json:"features"`
}

// ByName возвращает функцию по имени.
func (d *Description) ByName(name string) *Feature {
	for _, f := range d.Features {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// node - элемент XML с атрибутами, текстом и вложенными элементами.
type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []node     `xml:",any"`
}

func (n *node) attr(name string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value, true
		}
	}
	return "", false
}

func (n *node) walk(fn func(*node)) {
	fn(n)
	for i := range n.Children {
		n.Children[i].walk(fn)
	}
}

// Parse объединяет FeatureMapping.xml (имена) и DeviceDescription.xml (функции прибора).
func Parse(featureMapping, deviceDescription []byte) (*Description, error) {
	var fm, dd node
	if err := decode(featureMapping, &fm); err != nil {
		return nil, fmt.Errorf("FeatureMapping.xml: %w", err)
	}
	if err := decode(deviceDescription, &dd); err != nil {
		return nil, fmt.Errorf("DeviceDescription.xml: %w", err)
	}

	names := map[uint32]string{}
	enums := map[uint32]map[int]string{}
	enumNames := map[uint32]string{}
	var walkErr error
	fm.walk(func(n *node) {
		if walkErr != nil {
			return
		}
		if v, ok := n.attr("refUID"); ok {
			uid, err := parseHex(v)
			if err != nil {
				walkErr = fmt.Errorf("refUID %q: %w", v, err)
				return
			}
			names[uid] = strings.TrimSpace(n.Text)
		}
		if v, ok := n.attr("refENID"); ok {
			id, err := parseHex(v)
			if err != nil {
				walkErr = fmt.Errorf("refENID %q: %w", v, err)
				return
			}
			values := map[int]string{}
			for i := range n.Children {
				m := &n.Children[i]
				rv, ok := m.attr("refValue")
				if !ok {
					continue
				}
				val, err := strconv.Atoi(rv)
				if err != nil {
					walkErr = fmt.Errorf("refValue %q: %w", rv, err)
					return
				}
				values[val] = strings.TrimSpace(m.Text)
			}
			enums[id] = values
			if key, ok := n.attr("enumKey"); ok {
				enumNames[id] = strings.TrimSpace(key)
			}
		}
	})
	if walkErr != nil {
		return nil, fmt.Errorf("FeatureMapping.xml: %w", walkErr)
	}

	d := &Description{Features: map[uint32]*Feature{}}
	dd.walk(func(n *node) {
		if walkErr != nil {
			return
		}
		v, ok := n.attr("uid")
		if !ok {
			return
		}
		uid, err := parseHex(v)
		if err != nil {
			walkErr = fmt.Errorf("uid %q: %w", v, err)
			return
		}
		if _, dup := d.Features[uid]; dup {
			return
		}
		f := &Feature{UID: uid, Name: names[uid], Kind: n.XMLName.Local, Attrs: map[string]string{}}
		for _, a := range n.Attrs {
			switch a.Name.Local {
			case "uid":
			case "enumerationType":
				id, err := parseHex(a.Value)
				if err != nil {
					walkErr = fmt.Errorf("enumerationType %q: %w", a.Value, err)
					return
				}
				f.Values, f.Enum = enums[id], enumNames[id]
			default:
				f.Attrs[a.Name.Local] = a.Value
			}
		}
		if len(f.Attrs) == 0 {
			f.Attrs = nil
		}
		d.Features[uid] = f
	})
	if walkErr != nil {
		return nil, fmt.Errorf("DeviceDescription.xml: %w", walkErr)
	}
	if len(d.Features) == 0 {
		return nil, errors.New("DeviceDescription.xml: нет ни одной функции")
	}
	return d, nil
}

func decode(data []byte, n *node) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Внешние сущности encoding/xml не загружает, поэтому описание не может обратиться к файлам или сети.
	dec.Strict = true
	return dec.Decode(n)
}

func parseHex(s string) (uint32, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(s, 16, 32)
	return uint32(v), err
}
