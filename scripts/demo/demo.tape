# The README demo. .github/workflows/demo.yml runs this with VHS on a device
# it has just set up, while a real check job runs, so every line on screen is
# printed by the real binary. Run it again there to re-record.

Output demo.gif

Set Shell "bash"
Set FontFamily "JetBrains Mono"
Set FontSize 20
Set Width 1200
Set Height 640
Set Padding 28
Set Framerate 20
Set TypingSpeed 55ms
Set CursorBlink false
Set Theme { "name": "Tentmaker", "background": "#1F1C19", "foreground": "#F3EEE3", "cursor": "#E07A5F", "selection": "#3A342E", "black": "#1F1C19", "red": "#E07A5F", "green": "#9DBE84", "yellow": "#E3B76A", "blue": "#88A9BF", "purple": "#B897B0", "cyan": "#8DBDB0", "white": "#F3EEE3", "brightBlack": "#7A7166", "brightRed": "#F0957D", "brightGreen": "#B4D19C", "brightYellow": "#EFCB8A", "brightBlue": "#A5C2D4", "brightPurple": "#CDB0C6", "brightCyan": "#A8D0C5", "brightWhite": "#FFFBF3" }

# A plain prompt in the accent red, then a clean screen.
Hide
Type `export PS1='\[\e[38;2;224;122;95m\]$\[\e[0m\] '`
Enter
Type "clear"
Enter
Sleep 500ms
Show

Sleep 800ms
Type "tentmaker status"
Sleep 400ms
Enter
Sleep 4.5s
Screenshot status.png

Ctrl+L
Sleep 400ms
Type "tentmaker budget"
Sleep 400ms
Enter
Sleep 4s

Ctrl+L
Sleep 400ms
Type "tentmaker doctor"
Sleep 400ms
Enter
Sleep 4.5s
