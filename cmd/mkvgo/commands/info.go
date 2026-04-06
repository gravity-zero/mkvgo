package commands

import "fmt"

func CmdInfo(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		PrintJSON(struct {
			Path        string `json:"path"`
			Title       string `json:"title"`
			DurationMs  int64  `json:"duration_ms"`
			MuxingApp   string `json:"muxing_app"`
			WritingApp  string `json:"writing_app"`
			Tracks      int    `json:"tracks"`
			Chapters    int    `json:"chapters"`
			Attachments int    `json:"attachments"`
			Tags        int    `json:"tags"`
		}{
			c.Path, c.Info.Title, c.DurationMs,
			c.Info.MuxingApp, c.Info.WritingApp,
			len(c.Tracks), len(c.Chapters), len(c.Attachments), len(c.Tags),
		})
		return
	}
	fmt.Printf("File:        %s\n", c.Path)
	fmt.Printf("Title:       %s\n", c.Info.Title)
	fmt.Printf("Duration:    %d ms\n", c.DurationMs)
	fmt.Printf("MuxingApp:   %s\n", c.Info.MuxingApp)
	fmt.Printf("WritingApp:  %s\n", c.Info.WritingApp)
	fmt.Printf("Tracks:      %d\n", len(c.Tracks))
	fmt.Printf("Chapters:    %d\n", len(c.Chapters))
	fmt.Printf("Attachments: %d\n", len(c.Attachments))
	fmt.Printf("Tags:        %d\n", len(c.Tags))
}

func CmdTracks(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		PrintJSON(c.Tracks)
		return
	}
	for _, t := range c.Tracks {
		fmt.Printf("#%d  %-8s  %-10s  lang=%-5s  name=%q", t.ID, t.Type, t.Codec, t.Language, t.Name)
		if t.Width != nil && t.Height != nil {
			fmt.Printf("  %dx%d", *t.Width, *t.Height)
		}
		if t.Channels != nil {
			fmt.Printf("  %dch", *t.Channels)
		}
		if t.IsDefault {
			fmt.Print("  [default]")
		}
		if t.IsForced {
			fmt.Print("  [forced]")
		}
		fmt.Println()
	}
}

func CmdChapters(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		PrintJSON(c.Chapters)
		return
	}
	if len(c.Chapters) == 0 {
		fmt.Println("No chapters found.")
		return
	}
	for _, ch := range c.Chapters {
		fmt.Printf("  %s  [%s - %s]\n", ch.Title, FmtMs(ch.StartMs), FmtMs(ch.EndMs))
	}
}

func CmdAttachments(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		type attJSON struct {
			ID       uint64 `json:"id"`
			Name     string `json:"name"`
			MIMEType string `json:"mime_type"`
			Size     int64  `json:"size"`
		}
		out := make([]attJSON, len(c.Attachments))
		for i, a := range c.Attachments {
			out[i] = attJSON{a.ID, a.Name, a.MIMEType, a.Size}
		}
		PrintJSON(out)
		return
	}
	if len(c.Attachments) == 0 {
		fmt.Println("No attachments found.")
		return
	}
	for _, a := range c.Attachments {
		fmt.Printf("  #%d  %s  (%s, %d bytes)\n", a.ID, a.Name, a.MIMEType, a.Size)
	}
}

func CmdTags(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		PrintJSON(c.Tags)
		return
	}
	if len(c.Tags) == 0 {
		fmt.Println("No tags found.")
		return
	}
	for i, tag := range c.Tags {
		target := tag.TargetType
		if target == "" {
			target = "global"
		}
		if tag.TargetID > 0 {
			fmt.Printf("Tag #%d  target=%s  track=%d\n", i+1, target, tag.TargetID)
		} else {
			fmt.Printf("Tag #%d  target=%s\n", i+1, target)
		}
		for _, st := range tag.SimpleTags {
			lang := st.Language
			if lang == "" {
				lang = "und"
			}
			fmt.Printf("  %s = %q  [%s]\n", st.Name, st.Value, lang)
		}
	}
}

func CmdProbe(path string) {
	c := OpenMKV(path)
	if JsonOutput {
		PrintJSON(c)
		return
	}
	fmt.Printf("File:        %s\n", c.Path)
	fmt.Printf("Title:       %s\n", c.Info.Title)
	fmt.Printf("Duration:    %s (%d ms)\n", FmtMs(c.DurationMs), c.DurationMs)
	fmt.Printf("MuxingApp:   %s\n", c.Info.MuxingApp)
	fmt.Printf("WritingApp:  %s\n", c.Info.WritingApp)
	if c.Info.DateUTC != nil {
		fmt.Printf("Date:        %s\n", c.Info.DateUTC.Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Println()

	fmt.Printf("Tracks (%d):\n", len(c.Tracks))
	for _, t := range c.Tracks {
		fmt.Printf("  #%d  %-8s  %-10s  lang=%-5s  name=%q", t.ID, t.Type, t.Codec, t.Language, t.Name)
		if t.Width != nil && t.Height != nil {
			fmt.Printf("  %dx%d", *t.Width, *t.Height)
		}
		if t.SampleRate != nil {
			fmt.Printf("  %.0fHz", *t.SampleRate)
		}
		if t.Channels != nil {
			fmt.Printf("  %dch", *t.Channels)
		}
		if t.BitDepth != nil {
			fmt.Printf("  %dbit", *t.BitDepth)
		}
		if t.IsDefault {
			fmt.Print("  [default]")
		}
		if t.IsForced {
			fmt.Print("  [forced]")
		}
		if len(t.CodecPrivate) > 0 {
			fmt.Printf("  codec_private=%d bytes", len(t.CodecPrivate))
		}
		fmt.Println()
	}

	if len(c.Chapters) > 0 {
		fmt.Printf("\nChapters (%d):\n", len(c.Chapters))
		for _, ch := range c.Chapters {
			fmt.Printf("  %s  [%s - %s]\n", ch.Title, FmtMs(ch.StartMs), FmtMs(ch.EndMs))
		}
	}
	if len(c.Attachments) > 0 {
		fmt.Printf("\nAttachments (%d):\n", len(c.Attachments))
		for _, a := range c.Attachments {
			fmt.Printf("  #%d  %s  (%s, %d bytes)\n", a.ID, a.Name, a.MIMEType, a.Size)
		}
	}
	if len(c.Tags) > 0 {
		fmt.Printf("\nTags (%d):\n", len(c.Tags))
		for _, tag := range c.Tags {
			for _, st := range tag.SimpleTags {
				fmt.Printf("  %s = %q\n", st.Name, st.Value)
			}
		}
	}
}
