package cli

// composedCommands are the composed analysis commands, gathered from the files
// that define them so that command.go's table needs one line and each family
// owns its own entries. Every entry follows the table's rules: an MCP tool with
// a parameter spec (or NoMCP with the reason), and a Run that goes through the
// daemon unless --no-daemon says otherwise.
func composedCommands() []*command {
	var out []*command
	out = append(out, verdictCommands()...) // type_hierarchy, check_references, rename_check, delete_check
	out = append(out, impactCommands()...)  // blast_radius, dead_code
	out = append(out, gitCommands()...)     // changed_symbols, churn, hotspots, related
	out = append(out, taskCommands()...)    // task_context
	out = append(out, guideCommands()...)   // guide, version
	return out
}
