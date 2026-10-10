import hmac
import io
import json
import os
import secrets
import sqlite3
import time
from datetime import date, timedelta
from pathlib import Path
from urllib.error import URLError
from flask import Flask, abort, flash, g, redirect, render_template, request, send_file, session, url_for
from werkzeug.security import check_password_hash, generate_password_hash
from . import imports, ranking, sync
from .store import Store, books, bump, get_meta, import_records, match, now, reorder, revision, set_meta

DEFAULT_PREFERENCES = {'library_weight': 1, 'length': 'any', 'mood': ''}
READING_STATES = ('to-read', 'reading', 'read', 'abandoned', 'archived', 'unreviewed')
LIBRARY_STATES = ('hold', 'ready', 'borrowed', 'returned', 'cancelled', 'available')


def create_app(config=None):
    app = Flask(__name__)
    demo = os.getenv('LOCAL_DEMO') == '1'
    app.config.update(
        SECRET_KEY=os.getenv('APP_SECRET_KEY', secrets.token_hex(32) if demo else ''),
        APP_PASSWORD=os.getenv('APP_PASSWORD', ''),
        DATABASE=os.getenv('DATABASE_PATH', 'data/next-chapter.sqlite3'),
        DEMO=demo, START_POLLER=os.getenv('START_POLLER', '1') == '1',
        MAX_CONTENT_LENGTH=2_000_000, SESSION_COOKIE_HTTPONLY=True,
        SESSION_COOKIE_SAMESITE='Lax', SESSION_COOKIE_SECURE=os.getenv('COOKIE_SECURE') == '1',
        PERMANENT_SESSION_LIFETIME=timedelta(days=7),
    )
    if config:
        app.config.update(config)
    if len(app.config['SECRET_KEY']) < 32:
        raise RuntimeError('APP_SECRET_KEY must contain at least 32 characters.')
    if not app.config['DEMO'] and len(app.config['APP_PASSWORD']) < 12:
        raise RuntimeError('APP_PASSWORD must contain at least 12 characters.')
    password_hash = generate_password_hash(app.config['APP_PASSWORD'] or secrets.token_hex(16))
    store = Store(app.config['DATABASE'])
    app.extensions['store'] = store

    @app.before_request
    def protect():
        if request.endpoint in ('health', 'static'):
            return
        if not app.config['DEMO'] and not session.get('authenticated') and request.endpoint != 'login':
            return redirect(url_for('login'))
        with store.connect() as db:
            g.page_revision = revision(db)
        if 'csrf' not in session:
            session['csrf'] = secrets.token_urlsafe(32)
        if request.method == 'POST':
            if not hmac.compare_digest(session['csrf'], request.form.get('csrf', '')):
                abort(400, 'Your form expired. Reload the page and try again.')

    @app.after_request
    def headers(response):
        response.headers['Content-Security-Policy'] = "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'"
        response.headers['X-Content-Type-Options'] = 'nosniff'
        response.headers['Referrer-Policy'] = 'same-origin'
        response.headers['X-Frame-Options'] = 'DENY'
        if request.endpoint != 'static':
            response.headers['Cache-Control'] = 'no-store'
        return response

    @app.context_processor
    def context():
        return {'csrf': session.get('csrf', ''), 'revision': getattr(g, 'page_revision', 0), 'demo': app.config['DEMO'],
                'reading_states': READING_STATES, 'library_states': LIBRARY_STATES}

    @app.errorhandler(ValueError)
    def invalid(exc):
        return render_template('error.html', message=str(exc)), 400

    @app.errorhandler(400)
    @app.errorhandler(404)
    @app.errorhandler(409)
    @app.errorhandler(413)
    @app.errorhandler(429)
    def http_error(exc):
        return render_template('error.html', message=exc.description), exc.code

    @app.errorhandler(sqlite3.IntegrityError)
    def duplicate(exc):
        return render_template('error.html', message='That book already exists, or the submitted data conflicts with an existing record.'), 409

    def current(db):
        if request.form.get('revision') != str(revision(db)):
            abort(409, 'Your library changed since this page loaded. Reload before making another change.')

    def preferences(db):
        return get_meta(db, 'preferences', DEFAULT_PREFERENCES)

    def save_plan(db, kind, payload):
        pid = secrets.token_urlsafe(20)
        db.execute('DELETE FROM plans WHERE created_at<?', (time.time() - 3600,))
        db.execute('INSERT INTO plans VALUES (?,?,?,?,?)', (pid, kind, json.dumps(payload), revision(db), time.time()))
        return pid

    @app.get('/healthz')
    def health():
        with store.connect() as db:
            db.execute('SELECT 1').fetchone()
        return {'status': 'ok'}

    @app.route('/login', methods=['GET', 'POST'])
    def login():
        if request.method == 'POST':
            with store.connect(write=True) as db:
                db.execute('DELETE FROM login_attempts WHERE at<?', (time.time() - 60,))
                attempts = db.execute('SELECT count(*) FROM login_attempts').fetchone()[0]
                if attempts >= 10:
                    abort(429, 'Too many login attempts. Wait a minute and try again.')
                db.execute('INSERT INTO login_attempts VALUES (?)', (time.time(),))
            if check_password_hash(password_hash, request.form.get('password', '')):
                session.clear()
                session['authenticated'] = True
                session['csrf'] = secrets.token_urlsafe(32)
                session.permanent = True
                return redirect(url_for('index'))
            flash('That password was not recognized.', 'error')
        return render_template('login.html')

    @app.post('/logout')
    def logout():
        session.clear()
        return redirect(url_for('login'))

    @app.get('/')
    def index():
        with store.connect() as db:
            all_books = books(db)
            prefs = preferences(db)
            undo = db.execute('SELECT revision FROM undo_queue WHERE id=1').fetchone()
            can_undo = bool(undo and undo[0] == revision(db))
        view = request.args.get('view', 'queue')
        if view not in ('queue', 'all', 'reading', 'read'):
            view = 'queue'
        selected = [b for b in all_books if view == 'all' or b['reading_status'] == ('to-read' if view == 'queue' else view)]
        query = request.args.get('q', '').strip().casefold()
        if query:
            selected = [b for b in selected if query in (b['title'] + ' ' + b['author'] + ' ' + b['tags']).casefold()]
        queue = [b for b in all_books if b['reading_status'] == 'to-read']
        positions = {b['id']: i + 1 for i, b in enumerate(queue)}
        return render_template('index.html', items=selected, all_books=all_books, view=view, query=query,
                               positions=positions, prefs=prefs, queue=queue, can_undo=can_undo,
                               pending=[b for b in queue if b['needs_placement']],
                               active_loans=sum(any(l['status'] in ('borrowed', 'ready') for l in b['loans']) for b in all_books))

    def book_fields(form):
        title, author = form.get('title', '').strip(), form.get('author', '').strip()
        if not title or not author or len(title) > 500 or len(author) > 300:
            raise ValueError('Enter a title (up to 500 characters) and author (up to 300 characters).')
        interest = int(form.get('interest', '3'))
        pages = int(form['pages']) if form.get('pages') else None
        series_number = float(form['series_number']) if form.get('series_number') else None
        if interest not in range(1, 6) or pages is not None and not 1 <= pages <= 99999:
            raise ValueError('Interest must be 1–5 and pages must be 1–99,999.')
        if series_number is not None and not 0 <= series_number <= 9999:
            raise ValueError('Series number must be between 0 and 9,999.')
        reading_status = form.get('reading_status', 'to-read')
        if reading_status not in READING_STATES:
            raise ValueError('Choose a valid reading status.')
        return {'title': title, 'author': author, 'identity': imports.identity(title, author),
                'interest': interest, 'pages': pages, 'series': form.get('series', '').strip()[:200],
                'series_number': series_number, 'tags': form.get('tags', '')[:500], 'reading_status': reading_status}

    @app.route('/books/new', methods=['GET', 'POST'])
    def new_book():
        if request.method == 'POST':
            fields = book_fields(request.form)
            with store.connect(write=True) as db:
                current(db)
                record = dict(fields, source='manual', isbn='', goodreads_id='', event=None)
                if match(db, record):
                    raise ValueError('This book is already in your library. Edit the existing entry instead.')
                import_records(db, [record])
                bid = match(db, record)['id']
                db.execute('UPDATE books SET interest=?,series=?,series_number=? WHERE id=?',
                           (fields['interest'], fields['series'], fields['series_number'], bid))
                bump(db)
                if fields['reading_status'] == 'to-read':
                    plan = ranking.propose(books(db, True), preferences(db), bid)
                    pid = save_plan(db, 'insert', {'rows': plan, 'candidate_id': bid})
                else:
                    pid = None
            flash('Book added. Review its suggested position below.' if pid else 'Book added.', 'success')
            return redirect(url_for('plan', pid=pid) if pid else url_for('index'))
        return render_template('book.html', book=None)

    @app.route('/books/<int:bid>', methods=['GET', 'POST'])
    def edit_book(bid):
        if request.method == 'POST':
            fields = book_fields(request.form)
            with store.connect(write=True) as db:
                current(db)
                old = db.execute('SELECT * FROM books WHERE id=?', (bid,)).fetchone()
                if not old:
                    abort(404)
                db.execute('UPDATE books SET ' + ','.join(f'{key}=?' for key in fields) + ' WHERE id=?', (*fields.values(), bid))
                if old['reading_status'] != fields['reading_status']:
                    db.execute('UPDATE books SET pinned=0,needs_placement=? WHERE id=?', (int(fields['reading_status'] == 'to-read'), bid))
                    queue = [b['id'] for b in books(db, True) if b['id'] != bid]
                    if fields['reading_status'] == 'to-read':
                        queue.append(bid)
                    reorder(db, queue)
                bump(db)
            flash('Book updated.', 'success')
            return redirect(url_for('index'))
        with store.connect() as db:
            book = next((b for b in books(db) if b['id'] == bid), None)
            events = [dict(r) for r in db.execute('SELECT * FROM events WHERE book_id=? ORDER BY at DESC', (bid,))]
        if not book:
            abort(404)
        return render_template('book.html', book=book, events=events)

    @app.post('/books/<int:bid>/action')
    def book_action(bid):
        action = request.form.get('action')
        with store.connect(write=True) as db:
            current(db)
            queue = books(db, True)
            ids = [b['id'] for b in queue]
            if bid not in ids:
                raise ValueError('This book is not in your queue.')
            i = ids.index(bid)
            if action == 'pin':
                db.execute('UPDATE books SET pinned=1-pinned WHERE id=?', (bid,))
            elif action in ('up', 'down'):
                target = i + (-1 if action == 'up' else 1)
                if target < 0 or target >= len(ids):
                    raise ValueError('The book is already at the end of the queue.')
                if queue[i]['pinned'] or queue[target]['pinned']:
                    raise ValueError('Unpin the affected book before moving across its position.')
                ids[i], ids[target] = ids[target], ids[i]
                reorder(db, ids)
                db.execute('UPDATE books SET needs_placement=0 WHERE id=?', (bid,))
            elif action == 'placed':
                db.execute('UPDATE books SET needs_placement=0 WHERE id=?', (bid,))
            else:
                raise ValueError('Unknown action.')
            bump(db)
        return redirect(url_for('index'))

    @app.post('/books/<int:bid>/loan')
    def loan(bid):
        with store.connect(write=True) as db:
            current(db)
            book = db.execute('SELECT * FROM books WHERE id=?', (bid,)).fetchone()
            if not book:
                abort(404)
            source, status = request.form.get('source'), request.form.get('status')
            if source not in ('nypl', 'libby') or status not in LIBRARY_STATES:
                raise ValueError('Choose a valid library and status.')
            fmt = request.form.get('format', 'print').strip()[:100]
            if not fmt:
                raise ValueError('Choose a format.')
            due = request.form.get('due_date', '')
            if due:
                date.fromisoformat(due)
            record = dict(book)
            record.update(source=source, event={'status': status, 'activity': status, 'at': now(), 'due_date': due, 'format': fmt})
            import_records(db, [record])
            bump(db)
        flash('Library event recorded. Reading status stays the same.', 'success')
        return redirect(url_for('edit_book', bid=bid))

    @app.post('/suggest')
    def suggest():
        with store.connect(write=True) as db:
            current(db)
            bid = int(request.form['candidate_id']) if request.form.get('candidate_id') else None
            rows = ranking.propose(books(db, True), preferences(db), bid)
            if not rows:
                raise ValueError('Add books to your queue first.')
            pid = save_plan(db, 'insert' if bid else 'reorder', {'rows': rows, 'candidate_id': bid})
        return redirect(url_for('plan', pid=pid))

    @app.route('/plans/<pid>', methods=['GET', 'POST'])
    def plan(pid):
        with store.connect(write=request.method == 'POST') as db:
            row = db.execute('SELECT * FROM plans WHERE id=?', (pid,)).fetchone()
            if not row:
                abort(404, 'This preview has already been applied or has expired.')
            if time.time() - row['created_at'] > 3600:
                abort(409, 'This preview expired. Create a fresh suggestion or import.')
            payload = json.loads(row['payload'])
            stale = row['revision'] != revision(db)
            if request.method == 'POST':
                if stale:
                    abort(409, 'Your library changed. Generate a fresh preview before applying it.')
                if row['kind'] == 'import':
                    added, count = import_records(db, payload['records'])
                    bump(db)
                    flash(f'Imported {added} new books and {count} new library events. Existing reading status and queue order were preserved.', 'success')
                else:
                    old_queue = [{'id': b['id'], 'needs_placement': b['needs_placement']} for b in books(db, True)]
                    reorder(db, [b['id'] for b in payload['rows']])
                    if payload.get('candidate_id'):
                        db.execute('UPDATE books SET needs_placement=0 WHERE id=?', (payload['candidate_id'],))
                    else:
                        db.execute("UPDATE books SET needs_placement=0 WHERE reading_status='to-read'")
                    bump(db)
                    db.execute('INSERT OR REPLACE INTO undo_queue VALUES (1,?,?)', (json.dumps(old_queue), revision(db)))
                    flash('Your new order is saved. You can undo it until your next library change.', 'success')
                db.execute('DELETE FROM plans WHERE id=?', (pid,))
                return redirect(url_for('index'))
        return render_template('plan.html', kind=row['kind'], payload=payload, stale=stale)

    @app.post('/undo')
    def undo():
        with store.connect(write=True) as db:
            current(db)
            saved = db.execute('SELECT * FROM undo_queue WHERE id=1').fetchone()
            if not saved or saved['revision'] != revision(db):
                abort(409, 'Undo is no longer available because your library changed.')
            previous = json.loads(saved['payload'])
            reorder(db, [b['id'] for b in previous])
            for b in previous:
                db.execute('UPDATE books SET needs_placement=? WHERE id=?', (b['needs_placement'], b['id']))
            db.execute('DELETE FROM undo_queue')
            bump(db)
        flash('Previous order restored.', 'success')
        return redirect(url_for('index'))

    @app.route('/imports', methods=['GET', 'POST'])
    def import_page():
        if request.method == 'POST':
            upload = request.files.get('file')
            if not upload:
                raise ValueError('Choose a CSV file.')
            records, errors = imports.parse_csv(upload.read(2_000_001), request.form.get('source'))
            if errors:
                return render_template('imports.html', errors=errors[:30], error_count=len(errors)), 400
            with store.connect(write=True) as db:
                current(db)
                pid = save_plan(db, 'import', {'records': records, 'source': request.form['source']})
            return redirect(url_for('plan', pid=pid))
        return render_template('imports.html', errors=[])

    @app.get('/examples/<source>.csv')
    def example(source):
        if source not in ('goodreads', 'libby', 'nypl'):
            abort(404)
        return send_file(Path(app.root_path).parent / 'examples' / f'{source}.csv', as_attachment=True)

    @app.route('/settings', methods=['GET', 'POST'])
    def settings_page():
        if request.method == 'POST':
            weight = float(request.form.get('library_weight', '1'))
            length = request.form.get('length', 'any')
            if weight not in (0, 0.5, 1, 2) or length not in ('any', 'short', 'long'):
                raise ValueError('Choose valid ranking preferences.')
            user_id = request.form.get('user_id', '').strip()
            shelf = request.form.get('shelf', 'to-read').strip()
            import re
            if user_id and (not re.fullmatch(r'\d{1,20}', user_id) or not re.fullmatch(r'[a-zA-Z0-9_-]{1,100}', shelf)):
                raise ValueError('Enter a numeric Goodreads user ID and a valid shelf name.')
            if 'enabled' in request.form and not user_id:
                raise ValueError('Enter your Goodreads user ID before enabling polling.')
            with store.connect(write=True) as db:
                current(db)
                set_meta(db, 'preferences', {'library_weight': weight, 'length': length, 'mood': request.form.get('mood', '').strip()[:100]})
                set_meta(db, 'sync', {'user_id': user_id, 'shelf': shelf or 'to-read', 'enabled': 'enabled' in request.form})
                bump(db)
            flash('Preferences saved.', 'success')
            return redirect(url_for('settings_page'))
        with store.connect() as db:
            prefs, connection, last_sync = preferences(db), get_meta(db, 'sync', {}), get_meta(db, 'last_sync')
        return render_template('settings.html', prefs=prefs, connection=connection, last_sync=last_sync)

    @app.post('/sync')
    def sync_now():
        try:
            added = sync.sync_once(store)
            flash(f'Goodreads checked. {added} new books need placement.', 'success')
        except (ValueError, URLError, TimeoutError, OSError):
            flash('Goodreads feed unavailable. Check that your shelf is public, or use CSV import.', 'error')
        return redirect(url_for('settings_page'))

    @app.get('/history')
    def history():
        with store.connect() as db:
            events = [dict(row) for row in db.execute('''SELECT events.*, books.title, books.author FROM events
                JOIN books ON books.id=events.book_id ORDER BY events.at DESC, events.id DESC LIMIT 500''')]
        return render_template('history.html', events=events)

    @app.get('/export.json')
    def export():
        with store.connect() as db:
            data = {'schema_version': 1, 'exported_at': now(), 'books': books(db),
                    'events': [dict(row) for row in db.execute('SELECT * FROM events ORDER BY at')],
                    'preferences': preferences(db)}
        return send_file(io.BytesIO(json.dumps(data, indent=2).encode()), mimetype='application/json',
                         download_name='next-chapter-library.json', as_attachment=True)

    @app.get('/backup.sqlite3')
    def backup():
        # SQLite backup API produces a consistent snapshot even while WAL writes happen.
        import tempfile
        with tempfile.TemporaryDirectory() as folder:
            target = Path(folder) / 'backup.sqlite3'
            with store.connect() as db:
                out = sqlite3.connect(target)
                try:
                    db.backup(out)
                    out.execute('DELETE FROM plans')
                    out.execute('DELETE FROM login_attempts')
                    out.commit()
                finally:
                    out.close()
            data = target.read_bytes()
        return send_file(io.BytesIO(data), mimetype='application/octet-stream', download_name='next-chapter.sqlite3', as_attachment=True)

    @app.post('/demo')
    def seed_demo():
        if not app.config['DEMO']:
            abort(404)
        with store.connect(write=True) as db:
            current(db)
            if books(db):
                raise ValueError('Example books can only be loaded into an empty demo library.')
            records = []
            for title, author, pages, tags in [
                ('The Overstory', 'Richard Powers', 502, 'nature, fiction'),
                ('A Psalm for the Wild-Built', 'Becky Chambers', 160, 'hopeful, science fiction'),
                ('Braiding Sweetgrass', 'Robin Wall Kimmerer', 390, 'nature, essays'),
                ('Piranesi', 'Susanna Clarke', 245, 'mysterious, fantasy'),
                ('The Creative Act', 'Rick Rubin', 432, 'creativity, nonfiction')]:
                records.append({'title': title, 'author': author, 'pages': pages, 'tags': tags,
                                'source': 'demo', 'reading_status': 'to-read', 'event': None})
            import_records(db, records)
            queue = books(db, True)
            db.execute('UPDATE books SET needs_placement=0')
            db.execute('UPDATE books SET pinned=1 WHERE id=?', (queue[0]['id'],))
            db.execute('UPDATE books SET interest=5,needs_placement=1 WHERE id=?', (queue[3]['id'],))
            record = dict(queue[2], source='nypl', event={'status': 'borrowed', 'activity': 'borrowed', 'format': 'print',
                                'at': now(), 'due_date': (date.today() + timedelta(days=4)).isoformat()})
            import_records(db, [record])
            bump(db)
        return redirect(url_for('index'))

    if app.config['START_POLLER'] and not app.config.get('TESTING'):
        app.extensions['poller_stop'] = sync.start_poller(store, app.logger)
    return app
