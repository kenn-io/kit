package deadline

import "time"

var production = struct{ Timeout time.Duration }{20 * time.Millisecond}

func productionHelper()            { helperDuration(20 * time.Millisecond) }
func helperDuration(time.Duration) {}
