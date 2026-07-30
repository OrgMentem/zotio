# Manuscript gate: resolve \cite{} keys against the library, catch the bad ones.
Output ../assets/demos/bibcheck.gif

Set FontSize 14
Set Width 1400
Set Height 500
Set Padding 12
Set TypingSpeed 30ms
Set Shell "bash"
Set Theme "Catppuccin Mocha"

Hide
Type "export PATH=$PWD/../../bin:$PATH ZOTIO_DEMO=1 NO_COLOR=" Enter
Type "clear" Enter
Show

Type "cat fixtures/paper.tex"
Enter
Sleep 2.5s
Type "zotio items bibcheck fixtures/paper.tex"
Enter
Sleep 5s

# Static card for directory listings (Terminal Trove et al.); the final frame of
# this tape is the most self-explanatory single screen zotio has.
Screenshot ../assets/demos/preview.png
