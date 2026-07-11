# The integrity hero: find a genuinely retracted paper in the demo library.
# Hits the live Crossref API (Retraction Watch data) — record with network.
Output ../assets/demos/retract-check.gif

Set FontSize 14
Set Width 1400
Set Height 300
Set Padding 12
Set TypingSpeed 30ms
Set Shell "bash"
Set Theme "Catppuccin Mocha"

Hide
Type "export PATH=$PWD/../../bin:$PATH ZOTIO_DEMO=1 NO_COLOR=" Enter
Type "clear" Enter
Show

Type "zotio items retract-check --limit 5"
Enter
Sleep 16s
Sleep 3s
