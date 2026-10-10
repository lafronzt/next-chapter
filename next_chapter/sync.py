"""Opt-in, best-effort Goodreads RSS ingestion. Never writes to Goodreads."""
import re
import threading
from urllib.parse import urlencode
from urllib.request import Request, build_opener, HTTPRedirectHandler
from .imports import parse_rss
from .store import get_meta, set_meta, import_records, bump, now


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise ValueError('Goodreads redirected the feed. Use CSV import instead.')


def fetch(user_id, shelf):
    if not re.fullmatch(r'\d{1,20}', user_id) or not re.fullmatch(r'[a-zA-Z0-9_-]{1,100}', shelf):
        raise ValueError('Use a numeric Goodreads user ID and a shelf name containing letters, digits, hyphens or underscores.')
    url = f'https://www.goodreads.com/review/list_rss/{user_id}?' + urlencode({'shelf': shelf, 'sort': 'date_added', 'order': 'd'})
    request = Request(url, headers={'User-Agent': 'NextChapter/0.1 (personal reading queue)', 'Accept': 'application/rss+xml'})
    with build_opener(NoRedirect()).open(request, timeout=15) as response:
        return parse_rss(response.read(2_000_001))


def sync_once(store):
    with store.connect() as db:
        settings = get_meta(db, 'sync', {})
    if not settings.get('user_id'):
        raise ValueError('Save your Goodreads user ID first.')
    records = fetch(settings['user_id'], settings.get('shelf', 'to-read'))
    with store.connect(write=True) as db:
        # A settings change while the network request was running cancels ingestion.
        if settings != get_meta(db, 'sync', {}):
            raise ValueError('Connection settings changed. Try again.')
        before = db.total_changes
        added, _ = import_records(db, records)
        if db.total_changes != before:
            bump(db)
        set_meta(db, 'last_sync', {'at': now(), 'message': f'Found {len(records)} feed entries; added {added} books.', 'ok': True})
    return added


def start_poller(store, logger):
    def run():
        # One worker per container. CSV baseline is necessary: RSS is truncated and delayed.
        while not stopped.wait(60):
            try:
                with store.connect() as db:
                    settings = get_meta(db, 'sync', {})
                    last = get_meta(db, 'last_poll', 0)
                import time
                if not settings.get('enabled') or time.time() - last < 3600:
                    continue
                with store.connect(write=True) as db:
                    set_meta(db, 'last_poll', time.time())
                sync_once(store)
            except Exception:
                logger.warning('Goodreads sync failed; CSV import remains available.')
                with store.connect(write=True) as db:
                    set_meta(db, 'last_sync', {'at': now(), 'message': 'Feed unavailable. Check that your shelf is public or use CSV import.', 'ok': False})
    stopped = threading.Event()
    thread = threading.Thread(target=run, daemon=True, name='goodreads-poller')
    thread.start()
    return stopped
