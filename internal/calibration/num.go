package calibration

import "strconv"

func itoa(n int) string         { return strconv.Itoa(n) }
func floatStr(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
