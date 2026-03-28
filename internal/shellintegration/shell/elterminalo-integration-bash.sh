# ElTerminalo Shell Integration for Bash
# Emits OSC 133 sequences to bracket commands for the terminal to parse.
# Only activates inside ElTerminalo (detected via $TERM_PROGRAM).

[[ "$TERM_PROGRAM" == "ElTerminalo" ]] || return 0
[[ -n "$__ELTERMINALO_SHELL_INTEGRATION" ]] && return 0
__ELTERMINALO_SHELL_INTEGRATION=1

__elterminalo_cmd_executed=0
__elterminalo_in_prompt=0

__elterminalo_osc133() {
  builtin printf '\033]133;%s\007' "$1"
}

__elterminalo_prompt_command() {
  local exit_code=$?
  __elterminalo_in_prompt=1

  if (( __elterminalo_cmd_executed )); then
    __elterminalo_osc133 "D;$exit_code"
    __elterminalo_cmd_executed=0
  fi

  __elterminalo_osc133 "A"

  # Call the original PROMPT_COMMAND if any
  if [[ -n "$__elterminalo_orig_prompt_command" ]]; then
    eval "$__elterminalo_orig_prompt_command"
  fi

  __elterminalo_in_prompt=0
}

__elterminalo_debug_trap() {
  # Skip if we're inside PROMPT_COMMAND itself
  if (( __elterminalo_in_prompt )); then
    return
  fi
  # Skip for subshells and sourced scripts
  if [[ "$BASH_COMMAND" == "$PROMPT_COMMAND" ]]; then
    return
  fi

  # Capture full command from history and encode for history tracking
  local full_cmd
  full_cmd=$(HISTTIMEFORMAT='' builtin history 1 | sed 's/^[ ]*[0-9]*[ ]*//')
  local cmd_b64
  cmd_b64=$(builtin printf '%s' "$full_cmd" | base64 | tr -d '\n')

  __elterminalo_osc133 "B"
  __elterminalo_osc133 "C;cmd=${cmd_b64}"
  __elterminalo_cmd_executed=1

  # Remove the trap after first command (re-set by prompt_command on next prompt)
  trap - DEBUG
}

# Save original PROMPT_COMMAND and replace with ours
__elterminalo_orig_prompt_command="$PROMPT_COMMAND"
PROMPT_COMMAND='__elterminalo_prompt_command'

# DEBUG trap for preexec equivalent — re-armed after each prompt
__elterminalo_arm_debug() {
  trap '__elterminalo_debug_trap' DEBUG
}

# Arm the debug trap at the end of each prompt cycle
PROMPT_COMMAND='__elterminalo_prompt_command; __elterminalo_arm_debug'

# Load built-in prompt (skips if starship detected)
__elterminalo_prompt="${ELTERMINALO_SHELL_INTEGRATION_DIR}/elterminalo-prompt-bash.sh"
[[ -f "$__elterminalo_prompt" ]] && source "$__elterminalo_prompt"
unset __elterminalo_prompt
