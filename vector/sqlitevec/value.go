package sqlitevec

import "go.kenn.io/kit/vector/internal/vectorliteral"

func vectorValue(vector []float32) (string, any, error) {
	return "vec_f32(?)", vectorliteral.Format(vector), nil
}
