"""Bounded CSV/RSS adapters. Imports never infer that borrowed means read."""
import csv
import hashlib
import io
import re
import unicodedata
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from xml.etree import ElementTree

MAX_ROWS = 10000


def normalized(value):
    return re.sub(r'\W+', '', unicodedata.normalize('NFKC', value).casefold())


def identity(title, author):
    return normalized(title) + '|' + normalized(author)


def isbn(value):
    cleaned = re.sub(r'[^0-9Xx]', '', value or '').upper()
    return cleaned if len(cleaned) in (10, 13) else ''


def timestamp(value):
    if not value:
        return ''
    value = str(value).strip()
    try:
        if re.fullmatch(r'\d{10}(\d{3})?', value):
            dt = datetime.fromtimestamp(int(value) / (1000 if len(value) == 13 else 1), timezone.utc)
        else:
            try:
                dt = datetime.fromisoformat(value.replace('Z', '+00:00'))
            except ValueError:
                dt = None
                for fmt in ('%Y/%m/%d', '%m/%d/%Y', '%B %d, %Y', '%d %b %Y'):
                    try:
                        dt = datetime.strptime(value, fmt)
                        break
                    except ValueError:
                        pass
                if dt is None:
                    dt = parsedate_to_datetime(value)
        return dt.replace(tzinfo=dt.tzinfo or timezone.utc).astimezone(timezone.utc).isoformat()
    except (ValueError, TypeError, OverflowError):
        raise ValueError(f'Unrecognized date: {value[:50]}. Use YYYY-MM-DD or ISO 8601.') from None


def get(row, *names):
    for name in names:
        value = row.get(normalized(name))
        if value is not None and value.strip():
            return value.strip()
    return ''


def status(value):
    key = normalized(value)
    mapping = {'borrowed': 'borrowed', 'loan': 'borrowed', 'loans': 'borrowed', 'checkout': 'borrowed',
               'checkedout': 'borrowed', 'renewed': 'borrowed', 'renewal': 'borrowed', 'renew': 'borrowed',
               'hold': 'hold', 'holds': 'hold', 'placedonhold': 'hold', 'requested': 'hold',
               'ready': 'ready', 'readyforpickup': 'ready', 'available': 'available',
               'returned': 'returned', 'return': 'returned', 'cancelled': 'cancelled', 'canceled': 'cancelled'}
    if key not in mapping:
        raise ValueError(f'Unknown library activity "{value[:40]}". Use borrowed, hold, ready, returned, or cancelled.')
    return mapping[key]


def parse_csv(data, source):
    if source not in ('goodreads', 'libby', 'nypl'):
        raise ValueError('Choose a supported import source.')
    try:
        text = data.decode('utf-8-sig')
    except UnicodeDecodeError:
        raise ValueError('Save the file as UTF-8 CSV before importing.') from None
    reader = csv.DictReader(io.StringIO(text))
    if not reader.fieldnames or 'title' not in [normalized(h) for h in reader.fieldnames]:
        raise ValueError('CSV needs a Title column. See the downloadable example files.')
    records, errors = [], []
    for line, original in enumerate(reader, 2):
        if line > MAX_ROWS + 1:
            raise ValueError(f'Import at most {MAX_ROWS:,} rows at a time.')
        row = {normalized(k): v for k, v in original.items() if k is not None}
        try:
            title, author = get(row, 'title'), get(row, 'author', 'authors', 'author name', 'creator')
            if not title or not author:
                raise ValueError('Title and Author are required.')
            if len(title) > 500 or len(author) > 300:
                raise ValueError('Title or author is too long.')
            pages = get(row, 'number of pages', 'pages')
            record = {'title': title, 'author': author, 'isbn': isbn(get(row, 'isbn13', 'isbn')),
                      'goodreads_id': get(row, 'book id') if source == 'goodreads' else '',
                      'pages': int(pages) if pages.isdigit() and 0 < int(pages) < 100000 else None,
                      'source': source, 'reading_status': 'to-read', 'tags': '', 'event': None}
            if source == 'goodreads':
                shelf = get(row, 'exclusive shelf')
                if shelf and shelf not in ('to-read', 'read', 'currently-reading'):
                    record['reading_status'] = 'archived'
                else:
                    record['reading_status'] = {'read': 'read', 'currently-reading': 'reading'}.get(shelf, 'to-read')
                record['tags'] = get(row, 'bookshelves')[:500]
            else:
                record['reading_status'] = 'unreviewed'
                activity = get(row, 'activity', 'action', 'status')
                event_time = timestamp(get(row, 'timestamp', 'date', 'event date', 'updated at'))
                if not event_time:
                    raise ValueError('A Timestamp or Date is required for history reconciliation.')
                record['event'] = {'status': status(activity), 'activity': activity[:100], 'at': event_time,
                                   'format': get(row, 'format', 'media', 'media type')[:100] or ('print' if source == 'nypl' else 'digital'),
                                   'due_date': timestamp(get(row, 'due date', 'due'))[:10]}
            records.append(record)
        except ValueError as exc:
            errors.append(f'Row {line}: {exc}')
    if not records and not errors:
        raise ValueError('The CSV contains no book rows.')
    return records, errors


def parse_rss(data):
    if len(data) > 2_000_000 or b'<!DOCTYPE' in data.upper() or b'<!ENTITY' in data.upper():
        raise ValueError('Unsupported or oversized Goodreads feed.')
    try:
        root = ElementTree.fromstring(data)
    except ElementTree.ParseError:
        raise ValueError('Goodreads returned an unreadable feed. Try the CSV import instead.') from None
    if root.tag != 'rss':
        raise ValueError('Goodreads did not return an RSS feed. Your shelf must be public.')
    records = []
    for item in root.findall('./channel/item')[:MAX_ROWS]:
        title = (item.findtext('book_title') or item.findtext('title') or '').strip()
        author = (item.findtext('author_name') or '').strip()
        if not title or not author:
            continue
        pages = item.findtext('book/num_pages') or item.findtext('num_pages') or ''
        records.append({'title': title[:500], 'author': author[:300],
                        'goodreads_id': (item.findtext('book_id') or '').strip(),
                        'isbn': isbn(item.findtext('isbn') or ''), 'pages': int(pages) if pages.isdigit() else None,
                        'tags': '', 'source': 'goodreads', 'reading_status': 'to-read', 'event': None})
    return records


def fingerprint(record):
    event = record['event']
    raw = '|'.join([record['source'], identity(record['title'], record['author']),
                    event['format'], event['status'], event['at'], event['due_date']])
    return hashlib.sha256(raw.encode()).hexdigest()
