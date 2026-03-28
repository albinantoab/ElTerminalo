# ElTerminalo ZDOTDIR bootstrap
# Restores the user's original ZDOTDIR, sources their config, then loads shell integration.

# Restore original ZDOTDIR
if [[ -n "$ELTERMINALO_ORIG_ZDOTDIR" ]]; then
  ZDOTDIR="$ELTERMINALO_ORIG_ZDOTDIR"
else
  unset ZDOTDIR
fi

# Source the user's real .zshenv
[[ -f "${ZDOTDIR:-$HOME}/.zshenv" ]] && source "${ZDOTDIR:-$HOME}/.zshenv"

# Load shell integration
if [[ -n "$ELTERMINALO_SHELL_INTEGRATION_DIR" && -f "$ELTERMINALO_SHELL_INTEGRATION_DIR/elterminalo-integration-zsh.sh" ]]; then
  source "$ELTERMINALO_SHELL_INTEGRATION_DIR/elterminalo-integration-zsh.sh"
fi
