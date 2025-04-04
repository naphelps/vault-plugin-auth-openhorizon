package openhorizon

import (
	"fmt"
)

// Logging function to provide a prefix for log records generated by this plugin.
var ohlog = func(v interface{}) string {
	return fmt.Sprintf("openhorizon auth plugin: %v", v)
}
