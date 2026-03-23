#!/bin/bash
# ═══════════════════════════════════════════════
#  EL TERMINALO — A fancy terminal text display
# ═══════════════════════════════════════════════

# Purple palette
P1='\033[38;5;129m'  # Deep purple
P2='\033[38;5;135m'  # Medium purple
P3='\033[38;5;141m'  # Light purple
P4='\033[38;5;177m'  # Lavender
P5='\033[38;5;183m'  # Pale lavender
P6='\033[38;5;219m'  # Pink-lavender
W='\033[1;37m'       # White
D='\033[0;90m'       # Dim
N='\033[0m'          # Reset

# Hide cursor
tput civis 2>/dev/null
trap 'tput cnorm 2>/dev/null; echo' EXIT

clear

cols=$(tput cols 2>/dev/null || echo 80)

# Border line
border() {
    local char="$1"
    local color="$2"
    local line=""
    local width=$(( cols < 60 ? cols : 60 ))
    local pad=$(( (cols - width) / 2 ))
    for ((i=0; i<width; i++)); do
        line+="$char"
    done
    printf "%*s${color}%s${N}\n" "$pad" "" "$line"
}

# Typewriter effect (display_len = visual width for centering)
typewriter() {
    local text="$1"
    local color="$2"
    local delay="${3:-0.03}"
    local display_len="${4:-${#text}}"
    local pad=$(( (cols - display_len) / 2 ))
    (( pad < 0 )) && pad=0
    printf "%*s" "$pad" ""
    for ((i=0; i<${#text}; i++)); do
        printf "${color}%s${N}" "${text:$i:1}"
        sleep "$delay"
    done
    echo
}

# Centered print (with optional display width override)
cprint() {
    local text="$1"
    local color="$2"
    local display_len="${3:-${#text}}"
    local pad=$(( (cols - display_len) / 2 ))
    (( pad < 0 )) && pad=0
    printf "%*s${color}%s${N}\n" "$pad" "" "$text"
}

# ── INTRO SPARKLE ──
echo
for i in 1 2 3; do
    border "·" "$D"
    sleep 0.08
done
echo
sleep 0.3

# ── MAIN BANNER ──
colors=("$P1" "$P2" "$P3" "$P4" "$P5" "$P6")

ASCII_ART=(
"  ███████╗██╗          ████████╗███████╗██████╗ ███╗   ███╗██╗███╗   ██╗ █████╗ ██╗      ██████╗  "
"  ██╔════╝██║          ╚══██╔══╝██╔════╝██╔══██╗████╗ ████║██║████╗  ██║██╔══██╗██║     ██╔═══██╗ "
"  █████╗  ██║             ██║   █████╗  ██████╔╝██╔████╔██║██║██╔██╗ ██║███████║██║     ██║   ██║ "
"  ██╔══╝  ██║             ██║   ██╔══╝  ██╔══██╗██║╚██╔╝██║██║██║╚██╗██║██╔══██║██║     ██║   ██║ "
"  ███████╗███████╗        ██║   ███████╗██║  ██║██║ ╚═╝ ██║██║██║ ╚████║██║  ██║███████╗╚██████╔╝ "
"  ╚══════╝╚══════╝        ╚═╝   ╚══════╝╚═╝  ╚═╝╚═╝     ╚═╝╚═╝╚═╝  ╚═══╝╚═╝  ╚═╝╚══════╝ ╚═════╝  "
)

for idx in "${!ASCII_ART[@]}"; do
    color="${colors[$((idx % ${#colors[@]}))]}"
    line="${ASCII_ART[$idx]}"
    len=${#line}
    pad=$(( (cols - len) / 2 ))
    (( pad < 0 )) && pad=0
    printf "%*s${color}%s${N}\n" "$pad" "" "$line"
    sleep 0.1
done

echo
sleep 0.2

# ── DECORATIVE DIVIDER ──
border "━" "$P1"
border "═" "$P3"
border "━" "$P1"

echo
sleep 0.2

# ── TAGLINE ── (use cprint with explicit visual widths to avoid Unicode miscount)
# ">> Command-Line Interface v1.0 <<"  = 38 visible chars
cprint ">> Command-Line Interface v1.0 <<" "$P4" 38
sleep 0.3
echo
# "--- Precision. Performance. Control. ---" = 42 visible chars
cprint "--- Precision. Performance. Control. ---" "$D" 42
echo

# ── STATS BOX ──
sleep 0.3

sys_val=$(uname -s)
usr_val=$(whoami)
shl_val="${SHELL##*/}"
dte_val=$(date +%Y-%m-%d)
tme_val=$(date +%H:%M:%S)

border "─" "$D"
echo

# Fixed-width box
print_row() {
    local label="$1"
    local value="$2"
    local inner
    inner=$(printf "  %-10s ............ %-14s  " "$label" "$value")
    local total_len=${#inner}
    local box_pad=$(( (cols - total_len - 2) / 2 ))
    printf "%*s${P1}│${N}${P5}  %-10s${D} ............ ${W}%-14s${N}  ${P1}│${N}\n" "$box_pad" "" "$label" "$value"
}

inner_width=42
top_line=$(printf '─%.0s' $(seq 1 $inner_width))
top_pad=$(( (cols - inner_width - 2) / 2 ))

printf "%*s${P1}┌%s┐${N}\n" "$top_pad" "" "$top_line"
print_row "System" "$sys_val"
print_row "User" "$usr_val"
print_row "Shell" "$shl_val"
print_row "Date" "$dte_val"
print_row "Time" "$tme_val"
printf "%*s${P1}└%s┘${N}\n" "$top_pad" "" "$top_line"

echo
border "─" "$D"

echo
cprint "Vamos!" "$P3" 6
echo