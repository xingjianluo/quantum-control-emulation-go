package compile

import "strconv"

func sortFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
