# Renders the docs hero GIF: zero-setup sandbox -> search cards -> integrity
# tables -> library stats bars -> goal resolution. Pure local demo data.
# Requires: bin/zotio built, demo sandbox seeded (make demos handles both).
Output ../assets/demos/demo-tour.gif

Set FontSize 14
Set Width 1000
Set Height 620
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
Sleep 3s
Hide
Type "clear" Enter
Show
Type 'zotio search "memory"'
Enter
Sleep 4.5s
Hide
Type "clear" Enter
Show
Type "zotio items duplicates"
Enter
Sleep 3s
Type "zotio library stats --years 5 --top 5"
Enter
Sleep 5s
Hide
Type "clear" Enter
Show
Type "zotio which 'retracted papers'"
Enter
Sleep 5s
