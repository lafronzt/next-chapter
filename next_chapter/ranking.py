"""Deterministic, explainable queue recommendations. No external AI service."""
from datetime import date


def score(book, preferences, today=None):
    today = today or date.today()
    interest = int(book.get('interest', 3))
    total = interest * 10
    reasons = [f'Interest {interest}/5']
    timing = preferences.get('library_weight', 1)
    best = 0
    for loan in book.get('loans', []):
        status = loan['status']
        bonus = {'borrowed': 15, 'ready': 25, 'available': 5}.get(status, 0)
        if status in ('borrowed', 'ready'):
            reasons.append('Already borrowed' if status == 'borrowed' else 'Ready for pickup')
            if loan.get('due_date'):
                days = (date.fromisoformat(loan['due_date']) - today).days
                if days <= 7:
                    bonus += 20
                    reasons.append('Deadline within 7 days' if days >= 0 else 'Deadline has passed')
        best = max(best, bonus)
    total += best * timing
    mood = preferences.get('mood', '').strip().casefold()
    if mood and mood in [tag.strip().casefold() for tag in book.get('tags', '').split(',')]:
        total += 15
        reasons.append(f'Matches your {mood} mood')
    length = preferences.get('length', 'any')
    pages = book.get('pages') or 0
    if pages and ((length == 'short' and pages <= 250) or (length == 'long' and pages >= 400)):
        total += 10
        reasons.append('Matches your preferred length')
    return total, reasons


def prerequisites(book, books):
    if not book.get('series') or book.get('series_number') is None:
        return set()
    return {b['id'] for b in books if b.get('series', '').casefold() == book['series'].casefold()
            and b.get('series_number') is not None and b['series_number'] < book['series_number']}


def propose(books, preferences, candidate_id=None, today=None):
    """Pins retain their absolute slots. Insertion preserves all other relative order."""
    books = list(books)
    scored = {b['id']: score(b, preferences, today) for b in books}
    original = {b['id']: i for i, b in enumerate(books)}
    pins = {i: b['id'] for i, b in enumerate(books) if b['pinned']}
    if candidate_id is not None:
        candidate = next((b for b in books if b['id'] == candidate_id), None)
        if not candidate or candidate['pinned']:
            raise ValueError('Choose an unpinned book in your queue.')
        rest = [b for b in books if b['id'] != candidate_id]
        ideal = next((i for i, b in enumerate(rest) if scored[candidate_id][0] > scored[b['id']][0]), len(rest))
        options = []
        for i in range(len(rest) + 1):
            trial = rest[:i] + [candidate] + rest[i:]
            pos = {b['id']: j for j, b in enumerate(trial)}
            if any(trial[j]['id'] != bid for j, bid in pins.items()):
                continue
            if any(pos[bid] > i for bid in prerequisites(candidate, books)):
                continue
            if any(candidate_id in prerequisites(b, books) and pos[b['id']] < i for b in rest):
                continue
            options.append((abs(i - ideal), i, trial))
        if not options:
            raise ValueError('Pins and series order leave no valid position. Unpin a book or check series numbers.')
        result = min(options, key=lambda x: (x[0], x[1]))[2]
    else:
        # Pinned books impose deadlines on their prerequisites. Schedule those first
        # when no slack remains; this avoids false conflicts from a greedy sort.
        result = []
        remaining = {b['id']: b for b in books}
        deps = {b['id']: prerequisites(b, books) for b in books}
        for slot in range(len(books)):
            done = {b['id'] for b in result}
            if slot in pins:
                chosen = remaining[pins[slot]]
                if deps[chosen['id']] - done:
                    raise ValueError('A pinned book precedes an earlier book in its series. Unpin it first.')
            else:
                eligible = [b for b in remaining.values() if not b['pinned'] and not (deps[b['id']] - done)]
                urgent = set()
                for deadline, bid in sorted(pins.items()):
                    if deadline <= slot:
                        continue
                    required = set().union(*(deps[pin_id] - done for d, pin_id in pins.items() if slot < d <= deadline))
                    available_slots = sum(j not in pins for j in range(slot, deadline))
                    unpinned_needed = {x for x in required if not remaining[x]['pinned']}
                    if len(unpinned_needed) > available_slots:
                        raise ValueError('Pinned positions conflict with series order. Unpin a book first.')
                    if unpinned_needed and len(unpinned_needed) == available_slots:
                        urgent = unpinned_needed
                        break
                if urgent:
                    eligible = [b for b in eligible if b['id'] in urgent]
                if not eligible:
                    raise ValueError('Pins conflict with series order. Unpin a book first.')
                chosen = max(eligible, key=lambda b: (scored[b['id']][0], -original[b['id']]))
            result.append(chosen)
            del remaining[chosen['id']]
    return [{'id': b['id'], 'title': b['title'], 'author': b['author'], 'old': original[b['id']] + 1,
             'new': i + 1, 'pinned': b['pinned'], 'score': scored[b['id']][0],
             'reasons': scored[b['id']][1] + (['Pinned position'] if b['pinned'] else [])}
            for i, b in enumerate(result)]
