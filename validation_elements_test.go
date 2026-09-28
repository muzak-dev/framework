package muzak

import (
	"net/http"
	"reflect"
	"slices"
	"testing"
)

// lineItem is a model held in each element of a collection.
type lineItem struct {
	Name string  `json:"name"`
	Ship address `json:"ship_to"`
}

func (i *lineItem) Validate(v *Validation) {
	v.String(&i.Name).Required()
	v.Nested(&i.Ship)
}

// order nests each of its items by the element's own address.
type order struct {
	Items []lineItem `json:"items"`
}

func (in *order) Validate(v *Validation) {
	for i := range in.Items {
		v.Nested(&in.Items[i])
	}
}

// fieldsOf lists the fields a rejected request's details name, in order.
func fieldsOf(t *testing.T, app *App, target, body string) []string {
	t.Helper()
	rec := do(t, app, "POST", target, body)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	var fields []string
	for _, detail := range decodeError(t, rec).Error.Details {
		fields = append(fields, detail.Field)
	}
	return fields
}

const goodShip = `"ship_to":{"street":"s","city":"c","postcode":"06000"}`

// TestNestedPerElementNamesThePosition is the regression test for a model
// nested once per element: its failures were reported as "name", which says
// nothing about which element to fix.
func TestNestedPerElementNamesThePosition(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/orders", func(ctx *Context, in order) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	got := fieldsOf(t, app, "/orders", `{"items":[
		{"name":"a",`+goodShip+`},
		{"name":"",`+goodShip+`},
		{"name":"c","ship_to":{"street":"s","city":"","postcode":"06000"}}
	]}`)
	want := []string{"items[1].name", "items[2].ship_to.city"}
	if !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
}

// pointerOrder holds its items behind pointers and nests them last first, which
// is the order the search for an element's position does not expect.
type pointerOrder struct {
	Items []*lineItem `json:"items"`
}

func (in *pointerOrder) Validate(v *Validation) {
	for i := len(in.Items) - 1; i >= 0; i-- {
		v.Nested(in.Items[i])
	}
}

func TestNestedPointerElementsNameThePosition(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/orders", func(ctx *Context, in pointerOrder) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	item := func(name string) string { return `{"name":"` + name + `",` + goodShip + `}` }
	got := fieldsOf(t, app, "/orders", `{"items":[`+item("a")+`,`+item("")+`,null,`+item("")+`,`+item("e")+`]}`)
	want := []string{"items[3].name", "items[1].name"}
	if !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
}

// basket holds its collection in an embedded struct, whose fields are the
// model's own as far as the client can tell.
type basket struct {
	basketLines
}

type basketLines struct {
	Lines []lineItem `json:"lines"`
}

func (in *basket) Validate(v *Validation) {
	for i := range in.Lines {
		v.Nested(&in.Lines[i])
	}
}

func TestNestedElementsOfAnEmbeddedCollection(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/baskets", func(ctx *Context, in basket) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	got := fieldsOf(t, app, "/baskets", `{"lines":[{"name":"",`+goodShip+`}]}`)
	if want := []string{"lines[0].name"}; !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
}

// strays nests models that no collection of its own holds: a copy made by
// ranging over values, and one built on the spot. Neither can be given a
// position, and both are still validated.
type strays struct {
	Values   []lineItem  `json:"values"`
	Pointers []*lineItem `json:"pointers"`
	Others   []address   `json:"others"`
	Hidden   []lineItem  `json:"-"`
}

func (in *strays) Validate(v *Validation) {
	for _, item := range in.Pointers {
		copied := *item
		v.Nested(&copied)
	}
	v.Nested(&lineItem{Ship: address{Street: "s", City: "c", Post: "06000"}})
	in.Hidden = []lineItem{{}}
	v.Nested(&in.Hidden[0])
}

func TestNestedModelsOutsideACollectionHaveNoPosition(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/strays", func(ctx *Context, in strays) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	got := fieldsOf(t, app, "/strays", `{"pointers":[{"name":"",`+goodShip+`}],"others":[{}]}`)
	want := []string{"name", "name", "name", "ship_to.street", "ship_to.city", "ship_to.postcode"}
	if !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
}

// TestCollectListsFindsOnlyCollectionsOfModels covers the walk that finds the
// collections a nested model can be traced back to.
func TestCollectListsFindsOnlyCollectionsOfModels(t *testing.T) {
	t.Parallel()
	type inner struct {
		Deep []address `json:"deep"`
	}
	type model struct {
		inner
		Values   []address   `json:"values"`
		Pointers []*address  `json:"pointers"`
		Names    []string    `json:"names"`
		Empty    []struct{}  `json:"empty"`
		Hidden   []address   `json:"-"`
		private  []address   //nolint:unused // present to prove unexported fields are skipped
		Grid     [][]address `json:"grid"`
	}

	lists := collectLists(reflect.TypeFor[model](), nil, 0, nil)
	var found [][]int
	for _, list := range lists {
		found = append(found, list.index)
	}
	want := [][]int{{0, 0}, {1}, {2}}
	if !slices.EqualFunc(found, want, slices.Equal[[]int]) {
		t.Errorf("collections found at %v, want %v", found, want)
	}
	if got := collectLists(reflect.TypeFor[string](), nil, 0, nil); got != nil {
		t.Errorf("a type that is not a struct gave %v", got)
	}
}
