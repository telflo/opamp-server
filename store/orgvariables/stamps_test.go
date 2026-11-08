package orgvariables

import "testing"

func level(value string, stamp int64) *Level { return &Level{Value: &value, Stamp: stamp} }

// reset is a reset row: an override Reset, kept with no value.
func reset(stamp int64) *Level { return &Level{Stamp: stamp} }

// The source is the first level with a value from the top down (the pin's
// variable group, the collector group's, the organization's); the stamp is
// the greatest among the rows read down to the source, the source included.
func TestChooseTakesTheGreatestStampFromTheTopDownToTheSource(t *testing.T) {
	organization := *level("o", 1)
	for _, c := range []struct {
		name         string
		pin, group   *Level
		organization Level
		value        string
		stamp        int64
	}{
		{"the organization's value alone", nil, nil, organization, "o", 1},
		{"an override set over the organization's value", nil, level("g", 2), organization, "g", 2},
		{"that override changed", nil, level("g2", 3), organization, "g2", 3},
		{"that override Reset", nil, reset(4), organization, "o", 4},
		{"set again on the reset row", nil, level("g3", 5), organization, "g3", 5},
		{"a Reset at the pin's level over a live group override", reset(6), level("g3", 5), organization, "g3", 6},
		{"the group's override changed later under the pin's reset row", reset(6), level("g4", 7), organization, "g4", 7},
		{"the organization's value changed under a live override", nil, level("g4", 7), *level("o2", 8), "g4", 7},
		{"the group's override Reset under a live pin override", level("p", 9), reset(10), organization, "p", 9},
		{"a pin override over a live group override", level("p", 9), level("g", 11), *level("o3", 12), "p", 9},
		{"both reset: the organization's, with the greatest stamp", reset(13), reset(12), *level("o3", 11), "o3", 13},
		{"a pin override with no group override", level("p", 3), nil, *level("o", 5), "p", 3},
		{"an empty override is a value", nil, level("", 2), organization, "", 2},
	} {
		value, stamp := Choose(c.pin, c.group, c.organization)
		if value != c.value || stamp != c.stamp {
			t.Errorf("%s: Choose = %q, %d; want %q, %d", c.name, value, stamp, c.value, c.stamp)
		}
	}
}

// The wanted stamp is the greatest of the stamps of the names the text uses
// (each from its chosen source) and the two bindings', and 0 for a text that
// uses no name; a name the organization doesn't define counts 0.
func TestWantedStampTakesTheChosenSourcesStamp(t *testing.T) {
	r := Resolved{Revision: 20, Values: map[string]string{}, Stamps: map[string]int64{}}
	add := func(name string, pin, group *Level, organization Level) {
		r.Values[name], r.Stamps[name] = Choose(pin, group, organization)
	}
	add("FROM_PIN", level("p", 7), level("g", 15), *level("o", 20)) // stamp 7: the hidden changes don't count
	add("FROM_GROUP", reset(4), level("g", 9), *level("o", 18))     // stamp 9
	add("FROM_ORGANIZATION", nil, reset(11), *level("o", 3))        // stamp 11: the group's Reset is seen
	add("UNCHANGED", nil, nil, *level("o", 2))                      // stamp 2
	for _, c := range []struct {
		name                     string
		names                    []string
		groupBinding, pinBinding int64
		want                     int64
	}{
		{"no name", nil, 30, 40, 0},
		{"no name, empty", []string{}, 30, 40, 0},
		{"the pin's source", []string{"FROM_PIN"}, 0, 0, 7},
		{"the group's source", []string{"FROM_GROUP"}, 0, 0, 9},
		{"the organization's source under a reset row", []string{"FROM_ORGANIZATION"}, 0, 0, 11},
		{"the greatest of the names", []string{"UNCHANGED", "FROM_PIN", "FROM_GROUP"}, 0, 0, 9},
		{"an undefined name counts 0", []string{"UNDEFINED"}, 0, 0, 0},
		{"an undefined name beside a defined one", []string{"UNDEFINED", "UNCHANGED"}, 0, 0, 2},
		{"the group's binding counts with a name", []string{"UNCHANGED"}, 12, 0, 12},
		{"the pin's binding counts with a name", []string{"UNDEFINED"}, 0, 13, 13},
		{"a name's stamp over the bindings", []string{"FROM_ORGANIZATION"}, 5, 6, 11},
	} {
		if got := WantedStamp(c.names, r, c.groupBinding, c.pinBinding); got != c.want {
			t.Errorf("%s: WantedStamp = %d; want %d", c.name, got, c.want)
		}
	}
	if got := WantedStamp([]string{"A"}, Resolved{}, 0, 0); got != 0 {
		t.Errorf("WantedStamp over nothing resolved = %d; want 0", got)
	}
}
