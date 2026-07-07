# Renders the README hero GIF: zero-setup sandbox -> health verdict -> goal resolution.
# Requires: bin/zotio built, demo sandbox seeded (make demos handles both).
Output ../assets/demos/demo-tour.gif

Set FontSize 15
Set Width 920
Set Height 560
Set Padding 12
Set TypingSpeed 30ms
Set Shell "bash"
Set Theme "Catppuccin Mocha"

Hide
Type "export PATH=$PWD/../../bin:$PATH ZOTIO_DEMO=1 NO_COLOR=" Enter
Type "clear" Enter
Show

Type "zotio demo"
Enter
Sleep 3.5s
Hide
Type "clear" Enter
Show
Type "zotio library health --for citation"
Enter
Sleep 5s
Type "zotio which 'retracted papers'"
Enter
Sleep 4s
