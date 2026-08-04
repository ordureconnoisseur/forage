package configstore

import (
	"reflect"
	"testing"
)

// Every Patch field must be applied.
//
// applyPatch is a hand-written per-field merge, and a new setting needs
// registering in about five places: the Config struct, the env bootstrap, the
// merge in config.go, the Patch struct here, this apply, and the API's field
// map. Missing THIS one is the only omission that fails silently: the API
// accepts the value, returns ok, and the setting reverts to its default with
// no error anywhere. That is exactly how stashIgnoreScreenshots shipped
// looking wired and doing nothing.
//
// So rather than remember, this walks Patch by reflection, sets every pointer
// field to a distinguishable value, and asserts each one survives the merge.
// A field added to Patch and forgotten in applyPatch fails here by name.
func TestApplyPatchCoversEveryField(t *testing.T) {
	patchT := reflect.TypeOf(Patch{})
	patch := reflect.New(patchT).Elem()

	// Fill every pointer field with a non-zero value.
	for i := 0; i < patchT.NumField(); i++ {
		f := patchT.Field(i)
		if f.Type.Kind() != reflect.Ptr || !patch.Field(i).CanSet() {
			continue
		}
		v := reflect.New(f.Type.Elem())
		switch f.Type.Elem().Kind() {
		case reflect.String:
			v.Elem().SetString("x")
		case reflect.Bool:
			v.Elem().SetBool(true)
		case reflect.Int, reflect.Int64:
			v.Elem().SetInt(7)
		case reflect.Float32, reflect.Float64:
			v.Elem().SetFloat(1.5)
		default:
			continue // not a scalar we know how to fabricate
		}
		patch.Field(i).Set(v)
	}

	var base StoredConfig
	applyPatch(&base, patch.Interface().(Patch))

	baseV := reflect.ValueOf(base)
	baseT := baseV.Type()
	var missed []string
	for i := 0; i < patchT.NumField(); i++ {
		name := patchT.Field(i).Name
		if patch.Field(i).IsZero() {
			continue // we did not set it, so we cannot judge it
		}
		bf, ok := baseT.FieldByName(name)
		if !ok {
			continue // Patch-only field with no stored counterpart
		}
		if bf.Type.Kind() == reflect.Ptr && baseV.FieldByName(name).IsNil() {
			missed = append(missed, name)
		}
	}
	if len(missed) > 0 {
		t.Errorf("applyPatch ignores %d field(s): %v\n"+
			"A patch field that is never applied is accepted by the API, "+
			"reported as saved, and silently reverts to its default.",
			len(missed), missed)
	}
}
