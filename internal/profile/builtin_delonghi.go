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
		Events: &Events{
			Button:     map[string]string{"DeLonghi.Event.BeverageFinished": "click", "DeLonghi.Event.LoginRequired": "long_press"},
			FoodLevel:  []string{"DeLonghi.Alarm.NoCoffee"},
			WaterLevel: []string{"DeLonghi.Alarm.TankLevel"},
			Empty:      []string{"DeLonghi.Alarm.NoCoffee"},
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
