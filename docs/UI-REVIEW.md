# Responsive UI review — November 17, 2026

Reviewed the main reading flows using a separate local database with synthetic
books, long titles and author names, multiple reading states, loans, holds,
notes, and dated progress. No real account or reader data was used.

| Finding | Change |
| --- | --- |
| Home progress fields shrank to about 22px at 320px wide; even “Progress” broke across lines. | Give progress and unit their own labeled grid fields; stack at the narrowest widths and give Save a full row. The 320px progress input now occupies the available 280px. |
| Settings forms had both panel padding and an extra 22px inside the form. | Remove nested form padding on narrow layouts. Shorter ranking option captions remain readable in the native select. |
| Book covers occupied an entire row before the title and controls. | Use compact covers beside the heading, with metadata and actions spanning the card below. |
| A fixed sidebar extended below a 720px-tall desktop viewport. | Compact decorative spacing on short laptops, make the sidebar independently scrollable for smaller windows, and use the bottom navigation up to 900px to keep tablet content from being squeezed beside it. |
| Progress-history rows reserved 130px beside every update for its edit link. | Put the edit disclosure below the update on narrow screens. |
| Small controls and 14px inputs were awkward for touch and could trigger mobile Safari focus zoom. | Use 44px minimum button/control heights and 16px editable field text on narrow layouts. |
| Sticky tabs/search could cover the top of a book after an anchor jump. | Reserve 144px above row targets on narrow screens. |
| The selected All books tab could begin outside the visible horizontal tab strip. | Scroll the selected tab into the strip without moving the page vertically. |
| Menus stayed open after tapping elsewhere, and the More menu could exceed landscape height. | Support outside-click and Escape dismissal, return keyboard focus to the trigger, and bound More to the viewport with its own scrolling. |

Validation:

- 112 page/viewport checks, plus four short-laptop navigation checks, covering 320, 390, 768, 900, 901, 1280 and 1440px
  widths in light and dark themes. No unintended horizontal overflow or
  squeezed progress fields in the checked states. The library tabs intentionally
  retain their own horizontal scrolling.
- Pages included Home, All books, To review, reading books, book edit/add,
  Preferences, imports, reading/library history, calendar, series, saved views,
  Hardcover, and the note-sharing preview.
- Checked Escape and outside-click dismissal, focus return, selected-tab
  visibility, and a scrollable More menu at 568×320.
- Saved 47% progress through the phone UI in the synthetic library and verified
  the saved value and selected unit. Checked numeric keypad/example updates.
- Go race tests, vet and staticcheck passed; formatting and whitespace checks
  passed. These are browser-emulated Chromium checks, not physical-device
  Safari or a complete accessibility audit.

Screenshots:

- [320px progress controls](ux-progress-mobile-light.png)
- [320px preference spacing](ux-settings-mobile-light.png)
- [390px book page, dark](ux-book-mobile-dark.png)
- [1280×720 desktop navigation](ux-desktop-short-light.png)


## To review follows unresolved choices

To review now includes both undecided reading status and queued books marked
Needs placement, regardless of whether they were added by hand or imported.
The count, search, pagination and queue numbers all use that combined view.
Want to read keeps a book here for placement; Find its place previews a move,
and Keep here accepts its current position. Bulk reading decisions are limited
to undecided books and cannot change the placement group. Tests cover those
boundaries and verify that opening the view or accepting the current spot does
not reorder the queue.

The view omits unrelated queue statistics and the full-queue reorder banner.
Bulk reading decisions are collapsed until requested. Ten additional layout
checks at 320, 390, 768, 901 and 1280px in both themes found no unintended
overflow. Screenshots: [overview](to-review-mobile-dark.png) and
[placement and reading actions](to-review-placement-mobile-dark.png).
