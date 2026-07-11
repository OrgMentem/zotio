# Renders the library wrapped demo GIF: the year-in-review scrolls past and
# rests on the highlights + share hint. Pure local demo data.
# Requires: bin/zotio built, demo sandbox seeded (make demos handles both).
Output ../assets/demos/wrapped.gif

Set FontSize 14
Set Width 940
Set Height 640
Set Padding 12
Set TypingSpeed 30ms
Set Shell "bash"
Set Theme "Catppuccin Mocha"

Hide
Type "export PATH=$PWD/../../bin:$PATH ZOTIO_DEMO=1 NO_COLOR=" Enter
Type "clear" Enter
Show

Type "zotio library wrapped"
Enter
Sleep 8s
