package main

import (
	"html/template"
	"strconv"
	"strings"
)

// templateFuncs are the helpers every page shares. They exist so a figure or a
// timestamp reads the same wherever it is shown, rather than each template
// inventing its own shape.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"sum":    formatSum,
		"signed": formatSigned,
		// A protocol moment arrives as epoch milliseconds. Shown raw it is
		// unreadable, and shown in UTC it is five hours from the clock the
		// operator is reading, so both are offered: the local reading, and the
		// figure itself for anyone matching it against a log.
		"moment": moment,
	}
}

// formatSum turns tiyin into so'm: grouped thousands and two decimals. Amounts
// travel through the protocol in tiyin, which is unreadable at the scale a
// register holds — 100000000 and 10000000 differ by a digit nobody counts.
func formatSum(tiyin int64) string {
	negative := tiyin < 0
	if negative {
		tiyin = -tiyin
	}

	whole := group(strconv.FormatInt(tiyin/100, 10))
	cents := strconv.FormatInt(tiyin%100, 10)
	if len(cents) == 1 {
		cents = "0" + cents
	}

	out := whole + "." + cents
	if negative {
		return "-" + out
	}
	return out
}

// formatSigned keeps the sign visible on a balance move, where the direction
// matters more than the figure.
func formatSigned(tiyin int64) string {
	if tiyin > 0 {
		return "+" + formatSum(tiyin)
	}
	return formatSum(tiyin)
}

// group inserts a thin gap every three digits from the right.
func group(digits string) string {
	if len(digits) <= 3 {
		return digits
	}

	var out strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		out.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if out.Len() > 0 {
			out.WriteString(" ")
		}
		out.WriteString(digits[i : i+3])
	}

	return out.String()
}
