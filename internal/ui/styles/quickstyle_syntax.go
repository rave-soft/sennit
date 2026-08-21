package styles

import (
	"charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
)

// quickStyleMarkdown fills in Markdown, the glamour style config used to
// render assistant markdown. See quickStyleQuietMarkdown for the muted
// variant used by "thinking" content.
func quickStyleMarkdown(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Markdown = ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: hex(o.fgMarkdown),
			},
			// One cell of breathing room on each side, so prose doesn't
			// sit flush against the markdown panel's background edge.
			Margin: new(uint(1)),
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color:  hex(o.fgMoreSubtle),
				Italic: new(true),
			},
			Indent:      new(uint(1)),
			IndentToken: new("│ "),
		},
		List: ansi.StyleList{
			LevelIndent: defaultListIndent,
		},
		// Headings render brighter than body text (which uses fgSubtle),
		// not dimmer — a heading in a muted accent color reads as weaker
		// than the prose under it and the hierarchy disappears.
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       hex(o.fgBase),
				Bold:        new(true),
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix:          " ",
				Suffix:          " ",
				Color:           hex(o.warningSubtle),
				BackgroundColor: hex(o.primary),
				Bold:            new(true),
			},
		},
		H2: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "## ",
			},
		},
		H3: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "### ",
			},
		},
		H4: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "#### ",
			},
		},
		H5: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "##### ",
			},
		},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "###### ",
				Color:  hex(o.successMostSubtle),
				Bold:   new(false),
			},
		},
		Strikethrough: ansi.StylePrimitive{
			CrossedOut: new(true),
		},
		Emph: ansi.StylePrimitive{
			Italic: new(true),
		},
		Strong: ansi.StylePrimitive{
			Bold: new(true),
		},
		HorizontalRule: ansi.StylePrimitive{
			Color:  hex(o.separator),
			Format: "\n--------\n",
		},
		Item: ansi.StylePrimitive{
			BlockPrefix: "• ",
		},
		Enumeration: ansi.StylePrimitive{
			BlockPrefix: ". ",
		},
		Task: ansi.StyleTask{
			StylePrimitive: ansi.StylePrimitive{},
			Ticked:         "[✓] ",
			Unticked:       "[ ] ",
		},
		Link: ansi.StylePrimitive{
			Color:     hex(o.syntaxLink),
			Underline: new(true),
		},
		LinkText: ansi.StylePrimitive{
			Color: hex(o.successMostSubtle),
			Bold:  new(true),
		},
		Image: ansi.StylePrimitive{
			Color:     hex(o.syntaxBuiltin),
			Underline: new(true),
		},
		ImageText: ansi.StylePrimitive{
			Color:  hex(o.fgMoreSubtle),
			Format: "Image: {{.text}} →",
		},
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: " ",
				Suffix: " ",
				Color:  hex(o.keyword),
			},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{
					Color: hex(o.bgLessVisible),
				},
				Margin: new(uint(defaultMargin)),
			},
			Chroma: &ansi.Chroma{
				Text: ansi.StylePrimitive{
					Color: hex(o.fgSubtle),
				},
				Error: ansi.StylePrimitive{
					Color:           hex(o.onPrimary),
					BackgroundColor: hex(o.error),
				},
				Comment: ansi.StylePrimitive{
					Color: hex(o.fgMostSubtle),
				},
				CommentPreproc: ansi.StylePrimitive{
					Color: hex(o.syntaxPreproc),
				},
				Keyword: ansi.StylePrimitive{
					Color: hex(o.info),
				},
				KeywordReserved: ansi.StylePrimitive{
					Color: hex(o.syntaxKeyword),
				},
				KeywordNamespace: ansi.StylePrimitive{
					Color: hex(o.syntaxKeyword),
				},
				KeywordType: ansi.StylePrimitive{
					Color: hex(o.syntaxType),
				},
				Operator: ansi.StylePrimitive{
					Color: hex(o.syntaxOperator),
				},
				Punctuation: ansi.StylePrimitive{
					Color: hex(o.warningSubtle),
				},
				Name: ansi.StylePrimitive{
					Color: hex(o.fgSubtle),
				},
				NameBuiltin: ansi.StylePrimitive{
					Color: hex(o.syntaxBuiltin),
				},
				NameTag: ansi.StylePrimitive{
					Color: hex(o.syntaxTag),
				},
				NameAttribute: ansi.StylePrimitive{
					Color: hex(o.syntaxAttribute),
				},
				NameClass: ansi.StylePrimitive{
					Color:     hex(o.syntaxClass),
					Underline: new(true),
					Bold:      new(true),
				},
				NameDecorator: ansi.StylePrimitive{
					Color: hex(o.syntaxDecorator),
				},
				NameFunction: ansi.StylePrimitive{
					Color: hex(o.successMostSubtle),
				},
				LiteralNumber: ansi.StylePrimitive{
					Color: hex(o.success),
				},
				LiteralString: ansi.StylePrimitive{
					Color: hex(o.syntaxString),
				},
				LiteralStringEscape: ansi.StylePrimitive{
					Color: hex(o.successMoreSubtle),
				},
				GenericDeleted: ansi.StylePrimitive{
					Color: hex(o.destructive),
				},
				GenericEmph: ansi.StylePrimitive{
					Italic: new(true),
				},
				GenericInserted: ansi.StylePrimitive{
					Color: hex(o.successMostSubtle),
				},
				GenericStrong: ansi.StylePrimitive{
					Bold: new(true),
				},
				GenericSubheading: ansi.StylePrimitive{
					Color: hex(o.fgMoreSubtle),
				},
				Background: ansi.StylePrimitive{
					BackgroundColor: hex(o.bgLessVisible),
				},
			},
		},
		Table: ansi.StyleTable{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{},
			},
		},
		DefinitionDescription: ansi.StylePrimitive{
			BlockPrefix: "\n ",
		},
	}
}
