# ElTerminalo Built-in Prompt for Bash
# A minimal, informative prompt styled for ElTerminalo.
# Only activates if no prompt framework (starship) is detected.

[[ -n "$STARSHIP_SHELL" ]] && return 0

# --- Colors ---
_elt_cyan='\[\033[36m\]'
_elt_green='\[\033[32m\]'
_elt_red='\[\033[31m\]'
_elt_magenta='\[\033[35m\]'
_elt_yellow='\[\033[33m\]'
_elt_white='\[\033[37m\]'
_elt_reset='\[\033[0m\]'

# --- Git info ---
__elt_git_prompt() {
  local branch
  branch=$(git symbolic-ref --short HEAD 2>/dev/null) || \
  branch=$(git rev-parse --short HEAD 2>/dev/null) || return

  local staged modified untracked status_str=""
  staged=$(git diff --cached --numstat 2>/dev/null | wc -l | tr -d ' ')
  modified=$(git diff --numstat 2>/dev/null | wc -l | tr -d ' ')
  untracked=$(git ls-files --others --exclude-standard 2>/dev/null | wc -l | tr -d ' ')

  (( staged > 0 ))   && status_str+="\033[32m+${staged}\033[0m"
  (( modified > 0 ))  && status_str+="${status_str:+ }\033[33m!${modified}\033[0m"
  (( untracked > 0 )) && status_str+="${status_str:+ }\033[31m?${untracked}\033[0m"

  printf ' \033[37mon\033[0m \033[35m%s\033[0m' "$branch"
  [[ -n "$status_str" ]] && printf ' [%b]' "$status_str"
}

# --- Timer ---
__elt_timer_start=0

__elt_timer_preexec() {
  __elt_timer_start=$SECONDS
}
trap '__elt_timer_preexec' DEBUG

__elt_timer_precmd() {
  local duration=""
  if (( __elt_timer_start > 0 )); then
    local elapsed=$(( SECONDS - __elt_timer_start ))
    __elt_timer_start=0
    if (( elapsed >= 3 )); then
      local mins=$(( elapsed / 60 ))
      local secs=$(( elapsed % 60 ))
      if (( mins > 0 )); then
        duration=" \033[33mtook ${mins}m ${secs}s\033[0m"
      else
        duration=" \033[33mtook ${secs}s\033[0m"
      fi
    fi
  fi
  __elt_duration="$duration"
}

# --- Shorten path ---
__elt_short_pwd() {
  local p="${PWD/#$HOME/\~}"
  echo "$p"
}

# --- Build prompt ---
__elt_set_prompt() {
  local last_exit=$?
  __elt_timer_precmd

  local arrow
  if (( last_exit == 0 )); then
    arrow="${_elt_green}❯${_elt_reset}"
  else
    arrow="${_elt_red}❯${_elt_reset}"
  fi

  local git_info
  git_info=$(__elt_git_prompt)

  PS1="${_elt_cyan}\$(__elt_short_pwd)${_elt_reset}${git_info}\${__elt_duration}
${arrow} "
}

PROMPT_COMMAND="__elt_set_prompt${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
