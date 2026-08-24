package i18n

import (
	"strconv"
	"strings"
	"time"
)

// Strftime formats a time with a pattern written the way locale files write
// them, taking the names of days and months from the locale.
//
// Go formats a time by example rather than by directive, which is pleasant to
// write and cannot do the one thing localization exists for: the reference
// layout hard-codes "January" and "Mon", so a Go layout can only ever produce
// English names. A strftime pattern names the field instead, which leaves this
// package free to fill it from date.month_names in whatever locale was asked
// for. It is also what the published locale corpus is written in, so a file
// taken from it works here unchanged.
//
// A pattern that begins with "go:" is handed to Go's own formatter instead,
// for the formats that are machine-readable rather than localized:
//
//	formats:
//	  iso: "go:2006-01-02T15:04:05Z07:00"
func (s *Store) Strftime(locale, pattern string, t time.Time) string {
	if layout, isGo := strings.CutPrefix(pattern, "go:"); isGo {
		return t.Format(layout)
	}

	buf := buffers.Get().(*[]byte)
	defer func() {
		*buf = (*buf)[:0]
		buffers.Put(buf)
	}()
	b := (*buf)[:0]

	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' || i+1 >= len(pattern) {
			b = append(b, pattern[i])
			continue
		}
		i++
		f := readFlags(pattern, &i)
		if i >= len(pattern) {
			b = append(b, '%')
			break
		}
		b = s.appendDirective(b, locale, pattern[i], t, f)
	}

	*buf = b
	return string(b)
}

// flags are the modifiers written between a percent and the field it names.
type flags struct {
	// pad is '-' for none, '_' for spaces and '0' for zeroes, or nought for
	// whatever the field pads with by default.
	pad byte
	// upper upcases the result, which is what "%^B" asks for.
	upper bool
	// width is the column the field is padded to, or nought for its own.
	width int
}

// readFlags consumes the modifiers before a directive, leaving the index on the
// directive itself.
func readFlags(pattern string, i *int) flags {
	var f flags
	for ; *i < len(pattern); *i++ {
		switch pattern[*i] {
		case '-', '_', '0':
			f.pad = pattern[*i]
		case '^':
			f.upper = true
		default:
			// A run of digits after the flags is the width the field pads to.
			start := *i
			for *i < len(pattern) && pattern[*i] >= '0' && pattern[*i] <= '9' {
				*i++
			}
			if *i > start {
				f.width, _ = strconv.Atoi(pattern[start:*i])
			}
			return f
		}
	}
	return f
}

// appendDirective writes the field one directive names.
//
// The switch is exhaustive over the directives a locale file uses, and a
// directive this package does not know is written out as it stands rather than
// swallowed, so that an unfamiliar pattern degrades into visible text instead
// of into a gap.
func (s *Store) appendDirective(b []byte, locale string, directive byte, t time.Time, f flags) []byte {
	switch directive {
	case '%':
		return append(b, '%')
	case 'n':
		return append(b, '\n')
	case 't':
		return append(b, '\t')

	case 'Y':
		return appendNumber(b, t.Year(), 4, '0', f)
	case 'C':
		return appendNumber(b, t.Year()/100, 2, '0', f)
	case 'y':
		return appendNumber(b, t.Year()%100, 2, '0', f)
	case 'm':
		return appendNumber(b, int(t.Month()), 2, '0', f)
	case 'd':
		return appendNumber(b, t.Day(), 2, '0', f)
	case 'e':
		return appendNumber(b, t.Day(), 2, ' ', f)
	case 'j':
		return appendNumber(b, t.YearDay(), 3, '0', f)

	case 'H':
		return appendNumber(b, t.Hour(), 2, '0', f)
	case 'k':
		return appendNumber(b, t.Hour(), 2, ' ', f)
	case 'I':
		return appendNumber(b, hour12(t), 2, '0', f)
	case 'l':
		return appendNumber(b, hour12(t), 2, ' ', f)
	case 'M':
		return appendNumber(b, t.Minute(), 2, '0', f)
	case 'S':
		return appendNumber(b, t.Second(), 2, '0', f)
	case 'L':
		return appendNumber(b, t.Nanosecond()/1e6, 3, '0', f)
	case 'N':
		return appendNumber(b, t.Nanosecond(), 9, '0', f)
	case 's':
		return strconv.AppendInt(b, t.Unix(), 10)

	case 'a':
		return appendText(b, s.dayName(locale, t, true), f)
	case 'A':
		return appendText(b, s.dayName(locale, t, false), f)
	case 'b', 'h':
		return appendText(b, s.monthName(locale, t, true), f)
	case 'B':
		return appendText(b, s.monthName(locale, t, false), f)
	case 'p':
		return appendText(b, strings.ToUpper(s.meridiem(locale, t)), f)
	case 'P':
		return appendText(b, s.meridiem(locale, t), f)

	case 'u':
		day := int(t.Weekday())
		if day == 0 {
			day = 7
		}
		return appendNumber(b, day, 1, '0', f)
	case 'w':
		return appendNumber(b, int(t.Weekday()), 1, '0', f)
	case 'U':
		return appendNumber(b, weekOfYear(t, time.Sunday), 2, '0', f)
	case 'W':
		return appendNumber(b, weekOfYear(t, time.Monday), 2, '0', f)
	case 'V':
		_, week := t.ISOWeek()
		return appendNumber(b, week, 2, '0', f)
	case 'G':
		year, _ := t.ISOWeek()
		return appendNumber(b, year, 4, '0', f)

	case 'z':
		return append(b, t.Format("-0700")...)
	case 'Z':
		return append(b, t.Format("MST")...)

	// The compound directives, each of which is the pattern it stands for.
	case 'D':
		return append(b, s.Strftime(locale, "%m/%d/%y", t)...)
	case 'F':
		return append(b, s.Strftime(locale, "%Y-%m-%d", t)...)
	case 'T':
		return append(b, s.Strftime(locale, "%H:%M:%S", t)...)
	case 'R':
		return append(b, s.Strftime(locale, "%H:%M", t)...)
	case 'r':
		return append(b, s.Strftime(locale, "%I:%M:%S %p", t)...)
	case 'v':
		return append(b, s.Strftime(locale, "%e-%b-%Y", t)...)
	case 'x':
		return append(b, s.Strftime(locale, s.format(locale, "date.formats.default", "%Y-%m-%d"), t)...)
	case 'X':
		return append(b, s.Strftime(locale, "%H:%M:%S", t)...)
	case 'c':
		return append(b, s.Strftime(locale, s.format(locale, "time.formats.default", "%a %b %e %H:%M:%S %Y"), t)...)

	default:
		// Unknown to this package, so written out rather than dropped.
		return append(append(b, '%'), directive)
	}
}

// hour12 is the hour on a clock face, where midnight and noon are both twelve.
func hour12(t time.Time) int {
	switch hour := t.Hour() % 12; hour {
	case 0:
		return 12
	default:
		return hour
	}
}

// weekOfYear counts weeks from the first given weekday of the year, which is
// what the %U and %W directives report.
func weekOfYear(t time.Time, from time.Weekday) int {
	offset := (int(t.Weekday()) - int(from) + 7) % 7
	return (t.YearDay() + 6 - offset) / 7
}

// appendNumber writes a number padded the way its directive and flags ask for.
func appendNumber(b []byte, value, width int, pad byte, f flags) []byte {
	if f.pad != 0 {
		pad = f.pad
	}
	if pad == '_' {
		// The underscore flag asks for space padding; it is the request, not
		// the character to pad with.
		pad = ' '
	}
	if f.width > 0 {
		width = f.width
	}
	digits := strconv.Itoa(value)
	if pad != '-' {
		for i := len(digits); i < width; i++ {
			b = append(b, pad)
		}
	}
	return append(b, digits...)
}

// appendText writes a name, upcased when the pattern asked for it.
func appendText(b []byte, text string, f flags) []byte {
	if f.upper {
		text = strings.ToUpper(text)
	}
	return append(b, text...)
}

// dayName returns the locale's name for a weekday.
func (s *Store) dayName(locale string, t time.Time, short bool) string {
	key := "date.day_names"
	if short {
		key = "date.abbr_day_names"
	}
	if names := s.stringList(locale, key); int(t.Weekday()) < len(names) {
		return names[t.Weekday()]
	}
	if short {
		return t.Format("Mon")
	}
	return t.Format("Monday")
}

// monthName returns the locale's name for a month.
//
// The arrays are one-based, with a nil first entry, so that the number of a
// month indexes it directly, which is how the locale corpora this package reads
// are written.
func (s *Store) monthName(locale string, t time.Time, short bool) string {
	key := "date.month_names"
	if short {
		key = "date.abbr_month_names"
	}
	if names := s.stringList(locale, key); int(t.Month()) < len(names) {
		if name := names[t.Month()]; name != "" {
			return name
		}
	}
	if short {
		return t.Format("Jan")
	}
	return t.Format("January")
}

// meridiem returns the locale's word for the half of the day.
func (s *Store) meridiem(locale string, t time.Time) string {
	if t.Hour() < 12 {
		return s.format(locale, "time.am", "am")
	}
	return s.format(locale, "time.pm", "pm")
}
