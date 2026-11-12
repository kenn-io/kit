package civiltime

// The tests load named zones, and builds with -trimpath cannot find Go's own
// copy of the database, so embed it for hosts without one (Windows).
import _ "time/tzdata"
