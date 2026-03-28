# ElTerminalo Built-in Prompt for Zsh
# Vibrant, icon-rich prompt. Auto-disables if starship/p10k/oh-my-zsh is active.

[[ -n "$STARSHIP_SHELL" ]] && return 0
[[ -n "$POWERLEVEL9K_VERSION" ]] && return 0
[[ -n "$ZSH" && -d "$ZSH" && -f "$ZSH/oh-my-zsh.sh" ]] && return 0

setopt PROMPT_SUBST
zmodload zsh/datetime 2>/dev/null

# ── State ──
__elt_cmd_timer=0
__elt_last_exit=0
__elt_rprompt_str=""

# ── Signal name ──
__elt_signal_name() {
  local sig=$(( $1 - 128 ))
  case $sig in
    1) echo "HUP";;  2) echo "INT";;  3) echo "QUIT";;  6) echo "ABRT";;
    9) echo "KILL";; 11) echo "SEGV";; 13) echo "PIPE";; 15) echo "TERM";;
    *) echo "SIG${sig}";;
  esac
}

# ── Timer hooks ──
__elt_preexec_timer() { __elt_cmd_timer=$EPOCHSECONDS; }

__elt_precmd_prompt() {
  __elt_last_exit=$?
  local rparts=()

  # Exit status
  if (( __elt_last_exit != 0 )); then
    if (( __elt_last_exit > 128 )); then
      rparts+=("%F{203}✘ $(__elt_signal_name $__elt_last_exit)%f")
    else
      rparts+=("%F{203}✘ ${__elt_last_exit}%f")
    fi
  fi

  # Duration
  if (( __elt_cmd_timer > 0 )); then
    local elapsed=$(( EPOCHSECONDS - __elt_cmd_timer ))
    __elt_cmd_timer=0
    if (( elapsed >= 3 )); then
      local mins=$(( elapsed / 60 )) secs=$(( elapsed % 60 ))
      if (( mins > 0 )); then
        rparts+=("%F{214}took  ${mins}m ${secs}s%f")
      else
        rparts+=("%F{214}took  ${secs}s%f")
      fi
    fi
  fi

  __elt_rprompt_str="${(j: :)rparts}"
}

# ── Git info ──
__elt_git_info() {
  local branch
  branch=$(git symbolic-ref --short HEAD 2>/dev/null) || \
  branch=$(git rev-parse --short HEAD 2>/dev/null) || return

  local staged modified untracked s=""
  staged=$(git diff --cached --numstat 2>/dev/null | wc -l | tr -d ' ')
  modified=$(git diff --numstat 2>/dev/null | wc -l | tr -d ' ')
  untracked=$(git ls-files --others --exclude-standard 2>/dev/null | wc -l | tr -d ' ')

  (( staged > 0 ))   && s+=" %F{78}+${staged}%f"
  (( modified > 0 ))  && s+=" %F{214}!${modified}%f"
  (( untracked > 0 )) && s+=" %F{39}?${untracked}%f"

  printf ' %%F{white}on%%f %%F{78} %s%%f%s' "$branch" "$s"
}

# ── Path: ~/dir/…/parent/Current ──
__elt_styled_path() {
  local p="${PWD/#$HOME/~}"
  local parts=("${(@s:/:)p}")
  local count=${#parts}

  if (( count <= 1 )); then
    printf '%%F{39}%s%%f' "$p"
    return
  fi

  local parent="" last="${parts[-1]}"

  if (( count <= 3 )); then
    for (( i=1; i < count; i++ )); do
      parent+="${parts[$i]}/"
    done
  else
    parent+="${parts[1]}/"
    for (( i=2; i < count - 1; i++ )); do
      parent+="${parts[$i]:0:1}/"
    done
  fi

  printf '%%F{245}%s%%f%%B%%F{39}%s%%f%%b' "$parent" "$last"
}

# ── Register hooks ──
preexec_functions+=(__elt_preexec_timer)
precmd_functions+=(__elt_precmd_prompt)

# ── Prompt ──
PROMPT='%F{240}─%f %F{39}%f $(__elt_styled_path)$(__elt_git_info)
%(?.%F{78}❯%f.%F{203}❯%f) '

RPROMPT='${__elt_rprompt_str}'
