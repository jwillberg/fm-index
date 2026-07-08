package sais

import "fmt"

// Int32 constructs a suffix array for integer text symbols in [0, textMax).
func Int32(text []int32, textMax int) ([]int32, error) {
	if textMax <= 0 {
		return nil, fmt.Errorf("sais: textMax must be positive")
	}
	for index, symbol := range text {
		if symbol < 0 || int(symbol) >= textMax {
			return nil, fmt.Errorf("sais: symbol %d at offset %d outside [0,%d)", symbol, index, textMax)
		}
	}

	suffixArray := make([]int32, len(text))
	sais_32(text, textMax, suffixArray, make([]int32, 2*textMax))
	return suffixArray, nil
}
