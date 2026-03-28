# ElTerminalo Shell Integration for Zsh
# Emits OSC 133 sequences to bracket commands for the terminal to parse.
# Only activates inside ElTerminalo (detected via $TERM_PROGRAM).

[[ "$TERM_PROGRAM" == "ElTerminalo" ]] || return 0
[[ -n "$__ELTERMINALO_SHELL_INTEGRATION" ]] && return 0
__ELTERMINALO_SHELL_INTEGRATION=1

__elterminalo_cmd_executed=0

__elterminalo_osc133() {
  builtin printf '\033]133;%s\007' "$1"
}

__elterminalo_precmd() {
  local exit_code=$?

  if (( __elterminalo_cmd_executed )); then
    __elterminalo_osc133 "D;$exit_code"
    __elterminalo_cmd_executed=0
  fi

  __elterminalo_osc133 "A"
}

__elterminalo_preexec() {
  __elterminalo_osc133 "B"
  __elterminalo_osc133 "C"
  __elterminalo_cmd_executed=1
}

# Defer hook registration until the first precmd fires.
# This ensures our hooks are appended AFTER .zshrc and all prompt frameworks
# (starship, p10k, oh-my-zsh) have finished initializing.
__elterminalo_bootstrap() {
  # Remove ourselves (one-shot bootstrap)
  precmd_functions=(${precmd_functions:#__elterminalo_bootstrap})

  # Register the real hooks at the END of the arrays
  precmd_functions+=(__elterminalo_precmd)
  preexec_functions+=(__elterminalo_preexec)

  # Run precmd immediately for this first prompt
  __elterminalo_precmd
}

precmd_functions+=(__elterminalo_bootstrap)
