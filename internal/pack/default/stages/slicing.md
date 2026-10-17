# Slicing

Input: the need.

Cut the need into slices, each delivering value on its own. One slice is
the normal case; cut more only where a part ships and pays off without the
rest. Never cut by layer or by project: a slice spans every project it
needs.

Produce markdown with one `## <slice id>: <title>` section per slice: its
value and the projects it touches. Submit with a data object
`{"slices": [{"id": "SL1", "title": "<title>", "value": "<the value it delivers alone>", "projects": ["<name>"]}]}`.
