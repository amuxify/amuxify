# How amuxify compares with similar tools

If you already run something that strips tracks or re-encodes video, you may
wonder where amuxify fits. This page puts amuxify next to the tools that come up
most often in the same conversations, so you can see what each one is for and
where they overlap. The short version: amuxify is a gate. It checks a file once
when it arrives, cleans it, and proves that nothing was lost. Most of the other
tools are pipelines that keep changing files over the life of a library.

The columns group tools by purpose rather than listing every project by name.
The tools in one column behave alike for the rows that matter here.

Legend: ✅ does this, ❌ does not, ⚠️ partly or only with extra setup, ➖ does not apply.

| | amuxify | nudebomb, mkvstrip, radarr-striptracks | PlexCleaner | Tdarr, Unmanic, FileFlows | mediachecker, video-check, integv |
|---|---|---|---|---|---|
| What it is for | Checks a file, removes unwanted metadata and tracks, rebuilds it as a clean MKV and proves nothing was lost | Removes audio and subtitle tracks in languages you do not want | Tidies MKV files so Plex plays them well | Runs re-encoding jobs across a whole library | Reports whether a file decodes cleanly |
| Which tool writes the MKV | mkvmerge only | mkvmerge or mkvpropedit | mkvmerge, ffmpeg or HandBrake | ffmpeg or HandBrake | ➖ never writes files |
| Hashes every kept stream and compares it with the source | ✅ | ❌ | ❌ | ❌ | ➖ |
| Checks that the file still decodes | ✅ start and end by default, the whole file on request | ❌ | ⚠️ optional, whole file | ⚠️ optional | ✅ whole file |
| Spots executables and polyglot files | ✅ | ❌ | ❌ | ❌ | ❌ |
| Looks inside attachments such as fonts and cover art for hidden payloads | ✅ | ❌ | ❌ | ❌ | ❌ |
| Finds links in tags, chapter names and subtitle text | ✅ | ❌ | ❌ | ❌ | ❌ |
| Finds purchase and account atoms in MP4 files | ✅ | ❌ | ❌ | ❌ | ❌ |
| Never overwrites, writes to a temp file first, refuses symlinks | ✅ all three, each covered by a test | ⚠️ depends on the tool | ⚠️ some of it | ⚠️ some of it | ➖ |
| Runs as a post-processing script with stable exit codes | ✅ | ⚠️ some of them | ❌ | ❌ | ⚠️ some of them |
| Reads the SABnzbd, NZBGet, Sonarr and Radarr environment directly | ✅ | ❌ | ❌ | ❌ | ❌ |
| Re-encodes video or audio | ❌ never | ❌ | ⚠️ optional | ✅ that is its job | ❌ |
| Has a web interface | ❌ | ❌ | ❌ | ✅ | ❌ |

## Which one should you use?

When the files come from a source you trust and you only want to drop
languages, a nudebomb-style tool is lighter and does that job well. When you
want to re-encode a library to save space, Tdarr, Unmanic and FileFlows are
built for exactly that. When you want to know whether a file plays at all,
mediachecker and its relatives will tell you.

amuxify belongs in front of all of them. Run it when a file arrives, let it
refuse the file or clean it, and hand the clean result to whatever comes next.
