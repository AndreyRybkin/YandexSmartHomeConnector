package profile

import "strconv"

// Встроенный профиль кофемашины De'Longhi на платформе My Coffee Lounge. Имена - как у
// прибора из пакета delonghi; номера рецептов проверены на PrimaDonna Aromatic ECAM630.75.

// DeLonghiType - тип кофемашины De'Longhi.
const DeLonghiType = "DeLonghiCoffeeMaker"

// DeLonghiRecipePrefix - программа - рецепт машины: DeLonghi.Recipe.6 - американо.
const DeLonghiRecipePrefix = "DeLonghi.Recipe."

// DeLonghiRecipes - напиток Алисы -> номер рецепта по умолчанию; DeLonghiDrink - напиток
// для «включи» уже включённой машины.
var (
	DeLonghiRecipes = map[string]int{"espresso": 1, "americano": 6, "cappuccino": 15}
	DeLonghiDrink   = "americano"
)

const (
	// deLonghiAlarmPrefix и deLonghiPresent - имя тревоги машины и её значение, пока она есть.
	deLonghiAlarmPrefix = "DeLonghi.Alarm."
	deLonghiPresent     = "BSH.Common.EnumType.EventPresentState.Present"
)

// deLonghiServiceAlarms - тревоги, о которых датчик сообщает нажатием double_click: машина просит
// обслуживания или сообщает о сбое. У кнопки Яндекса три значения, поэтому оно одно на все тревоги.
var deLonghiServiceAlarms = []string{"BrewUnitClean", "BrokenGrinder", "Bubble", "Clean", "Descale", "Drain", "Fan", "Filter", "Fridge",
	"General", "GroundFine", "HeaterNTC", "Imbocco", "Infuser", "MotorPos", "UIMemory", "Waste"}

// deLonghiButtons - нажатия датчика: конец налива, нужен вход и появление тревоги.
func deLonghiButtons() map[string]string {
	buttons := map[string]string{"DeLonghi.Event.BeverageFinished": "click", "DeLonghi.Event.LoginRequired": "long_press"}
	for _, alarm := range deLonghiServiceAlarms {
		buttons[deLonghiAlarmPrefix+alarm+"="+deLonghiPresent] = "double_click"
	}
	return buttons
}

func init() {
	builtin[DeLonghiType] = &Profile{
		YandexType: "cooking.coffee_maker",
		OnOff:      "power",
		// Машина включена, пока CurrMode не 0 (выключена) и не 2 (выключается).
		Power: &Power{Setting: "DeLonghi.Setting.TurnOnOff", On: "2", Off: "1",
			State: "DeLonghi.Status.CurrMode", Idle: []string{"0", "2"}},
		Modes: []Mode{{Instance: "coffee_mode", Programs: map[string]string{
			DeLonghiRecipePrefix + "1": "espresso", DeLonghiRecipePrefix + "6": "americano", DeLonghiRecipePrefix + "15": "cappuccino",
		}}},
		DefaultProgram: DeLonghiRecipePrefix + "6",
		// Значение есть только во время налива.
		Tile: []TileItem{{Status: "DeLonghi.Status.Pouring", Label: "налито", Unit: percent}},
		Events: &Events{
			Button:    deLonghiButtons(),
			FoodLevel: []string{deLonghiAlarmPrefix + "NoCoffee"},
			// Без бака воды нет: уровень empty, а не low.
			WaterLevel: []string{deLonghiAlarmPrefix + "TankLevel", deLonghiAlarmPrefix + "TankPresence"},
			Empty:      []string{deLonghiAlarmPrefix + "NoCoffee", deLonghiAlarmPrefix + "TankPresence"},
		},
	}
}

// DeLonghiLayer - слой профиля кофемашины из настроек прибора: recipes (напиток Алисы ->
// номер рецепта) и drink для «включи» включённой машины. Пустое - значение по умолчанию.
func DeLonghiLayer(recipes map[string]int, drink string) *Profile {
	if len(recipes) == 0 {
		recipes = DeLonghiRecipes
	}
	if drink == "" {
		drink = DeLonghiDrink
	}
	p := &Profile{Modes: []Mode{{Instance: "coffee_mode", Programs: map[string]string{}}}}
	for name, n := range recipes {
		p.Modes[0].Programs[DeLonghiRecipePrefix+strconv.Itoa(n)] = name
	}
	if n, ok := recipes[drink]; ok {
		p.DefaultProgram = DeLonghiRecipePrefix + strconv.Itoa(n)
	}
	return p
}
