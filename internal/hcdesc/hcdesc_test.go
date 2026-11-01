package hcdesc

import "testing"

// Образцы повторяют структуру файлов из архива описания, значения выдуманы.
const testFeatureMapping = `<?xml version="1.0" encoding="UTF-8"?>
<featureMappingFile>
  <description/>
  <featureDescription>
    <feature refUID="0201">BSH.Common.Status.DoorState</feature>
    <feature refUID="0202">BSH.Common.Status.OperationState</feature>
    <feature refUID="1001">Cooking.Oven.Program.HeatingMode.HotAir</feature>
    <feature refUID="1002">Cooking.Oven.Option.SetpointTemperature</feature>
  </featureDescription>
  <errorDescription/>
  <enumDescriptionList>
    <enumDescription refENID="0001" enumKey="BSH.Common.EnumType.DoorState">
      <enumMember refValue="0">Open</enumMember>
      <enumMember refValue="1">Closed</enumMember>
    </enumDescription>
  </enumDescriptionList>
</featureMappingFile>`

const testDeviceDescription = `<?xml version="1.0" encoding="UTF-8"?>
<device>
  <description/>
  <statusList uid="0100">
    <status uid="0201" refCID="03" refDID="80" access="read" available="true" enumerationType="0001"/>
    <status uid="0202" refCID="03" refDID="80" access="read" available="true"/>
  </statusList>
  <programGroup uid="0300">
    <program uid="1001" available="true">
      <option refUID="1002" access="readWrite"/>
    </program>
  </programGroup>
  <optionList uid="0400">
    <option uid="1002" refCID="07" refDID="A1" min="30" max="275" stepSize="5" access="readWrite"/>
    <option uid="1003" access="read"/>
  </optionList>
</device>`

func TestParse(t *testing.T) {
	d, err := Parse([]byte(testFeatureMapping), []byte(testDeviceDescription))
	if err != nil {
		t.Fatal(err)
	}
	door := d.Features[0x0201]
	if door == nil || door.Name != "BSH.Common.Status.DoorState" || door.Kind != "status" {
		t.Fatalf("дверца: %+v", door)
	}
	if door.Values[0] != "Open" || door.Values[1] != "Closed" || door.EnumValue("Open") != "BSH.Common.EnumType.DoorState.Open" {
		t.Fatalf("значения дверцы: %v, перечисление %q", door.Values, door.Enum)
	}
	if door.Attrs["access"] != "read" || door.Attrs["refCID"] != "03" {
		t.Fatalf("атрибуты: %v", door.Attrs)
	}
	if _, ok := door.Attrs["uid"]; ok {
		t.Fatal("uid не должен попадать в атрибуты")
	}
	temp := d.ByName("Cooking.Oven.Option.SetpointTemperature")
	if temp == nil || temp.Kind != "option" || temp.Attrs["max"] != "275" {
		t.Fatalf("температура: %+v", temp)
	}
	if p := d.Features[0x1001]; p == nil || p.Kind != "program" {
		t.Fatalf("программа: %+v", p)
	}
	// Номера нет в FeatureMapping.xml - функция остаётся без имени.
	if f := d.Features[0x1003]; f == nil || f.Name != "" {
		t.Fatalf("функция без имени: %+v", f)
	}
	// Списки тоже имеют uid, а ссылка refUID внутри программы - не функция.
	if len(d.Features) != 8 {
		t.Fatalf("функций %d, нужно 8", len(d.Features))
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string][2]string{
		"битый XML":       {testFeatureMapping, "<device>"},
		"нет функций":     {testFeatureMapping, "<device/>"},
		"неверный uid":    {testFeatureMapping, `<device><status uid="zz"/></device>`},
		"неверный refUID": {`<m><feature refUID="xyz">A</feature></m>`, testDeviceDescription},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(c[0]), []byte(c[1])); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
}
