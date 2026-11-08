package orgvariables

// Stamps. Every change a collector's filled-in text can see stamps the exact
// row that changed with the organization's counter
// (organizations.variables_revision, moved on in the write's transaction): a
// variable's value, an override's row (set, changed, or Reset, which keeps
// the row with no value), a collector group's variable-group setting, a
// collector's pin or move. The poll records the newest stamp it filled in
// with as the collector's marker; the collector is In sync when its marker
// is at least WantedStamp. The variables store's statement and the fleet
// stores' wanted-stamp SQL mirror Choose and WantedStamp.

// Level is one level of a variable for a collector: the row it holds there,
// read for the value and its stamp. Value nil is a reset row: an override
// Reset, kept with no value, passed over for the value as a missing row is.
type Level struct {
	Value *string
	Stamp int64
}

// Choose is the rule for one variable. The levels are read from the top
// down: the pin's variable group's override row (nil: no row), the collector
// group's variable group's override row (nil: no row), then the variable
// itself. The source is the first level with a value, and the value is the
// source's. The stamp is the greatest stamp among the rows read from the top
// down to the source, the source included: a reset row above the source
// counts, so its Reset is seen; a row below the source doesn't, so a change
// it hides is not. The variable always has a value; a nil one reads as
// empty.
func Choose(pin, group *Level, organization Level) (value string, stamp int64) {
	for _, level := range []*Level{pin, group, &organization} {
		if level == nil {
			continue
		}
		stamp = max(stamp, level.Stamp)
		if level.Value != nil {
			return *level.Value, stamp
		}
	}
	return "", stamp
}

// WantedStamp is the marker a collector must have been served at least: 0
// when its text references no name (names, from RenderedLength); otherwise
// the greatest of each name's stamp in r (0 for a name r doesn't define) and
// the stamps of its collector group's variable-group setting (groupBinding)
// and of its pin's (pinBinding).
func WantedStamp(names []string, r Resolved, groupBinding, pinBinding int64) int64 {
	if len(names) == 0 {
		return 0
	}
	wanted := max(groupBinding, pinBinding)
	for _, name := range names {
		wanted = max(wanted, r.Stamps[name])
	}
	return wanted
}
