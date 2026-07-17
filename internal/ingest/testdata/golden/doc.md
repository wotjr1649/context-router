# Introduction

This document explains the golden fixture used by ChunkText tests. It has
enough body text under each heading to exercise line-block accumulation
and to prove that headings mark a new chunk when Markdown mode is on.
Every paragraph repeats similar filler sentences so the byte length is
predictable across edits to this fixture file.

## Background

Some background text goes here so this section is not trivially short.
The chunker should record this heading as the Title for any chunk that
starts inside this section, including chunks reached purely by the 4KB
byte-budget cut rather than by a heading boundary.

### Details

Deeper heading level three. This section closes out the document and
provides a third heading so the golden test can assert on three distinct
Title values across the produced chunks.
