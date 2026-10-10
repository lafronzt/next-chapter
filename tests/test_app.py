import io
import tempfile
import unittest
from datetime import date
from pathlib import Path
from unittest.mock import patch
from next_chapter import create_app
from next_chapter.imports import parse_csv, parse_rss
from next_chapter.ranking import propose
from next_chapter.store import Store, books, import_records, revision, set_meta
from next_chapter.sync import sync_once


def book(bid, interest=3, **extra):
    return dict(id=bid, title=f'Book {bid}', author='Author', pinned=False, interest=interest,
                pages=200, tags='', loans=[], **extra)


def record(title='Piranesi', source='goodreads', event=None):
    return dict(title=title, author='Susanna Clarke', source=source, event=event,
                reading_status='to-read', isbn='', goodreads_id='', pages=245, tags='')


class RankingTests(unittest.TestCase):
    def test_insert_preserves_other_relative_order(self):
        rows = propose([book(1, 4), book(2, 2), book(3, 3), book(4, 5)], {}, 4)
        self.assertEqual([r['id'] for r in rows], [4, 1, 2, 3])

    def test_pins_retain_absolute_positions_in_both_modes(self):
        queue = [book(1, 1), book(2, 2), book(3, 5), book(4, 4)]
        queue[1]['pinned'] = True
        for candidate in (None, 4):
            self.assertEqual(propose(queue, {}, candidate)[1]['id'], 2)

    def test_series_overrides_interest(self):
        queue = [book(2, 5, series='Test', series_number=2), book(1, 1, series='Test', series_number=1)]
        self.assertEqual([r['id'] for r in propose(queue, {})], [1, 2])

    def test_series_predecessor_scheduled_before_fixed_pin(self):
        queue = [book(1, 5), book(2, 5, series='Test', series_number=2), book(3, 1, series='Test', series_number=1)]
        queue[1]['pinned'] = True
        self.assertEqual([r['id'] for r in propose(queue, {})], [3, 2, 1])

    def test_impossible_series_pin_rejected(self):
        queue = [book(2, 5, series='Test', series_number=2), book(1, 1, series='Test', series_number=1)]
        queue[0]['pinned'] = True
        with self.assertRaises(ValueError):
            propose(queue, {})

    def test_multiple_series_pins_reserve_enough_earlier_slots(self):
        queue = [book(1, 5), book(2, 1, series='A', series_number=1),
                 book(3, 5, series='A', series_number=2),
                 book(4, 5, series='B', series_number=2),
                 book(5, 1, series='B', series_number=1)]
        queue[2]['pinned'] = queue[3]['pinned'] = True
        self.assertEqual([r['id'] for r in propose(queue, {})], [2, 5, 3, 4, 1])

    def test_library_deadline_weight_can_be_disabled(self):
        queue = [book(1, 4), book(2, 2)]
        queue[1]['loans'] = [{'status': 'borrowed', 'due_date': '2026-10-12'}]
        for weight, expected in [(1, 2), (0, 1)]:
            self.assertEqual(propose(queue, {'library_weight': weight}, today=date(2026, 10, 10))[0]['id'], expected)

    def test_equal_scores_preserve_order(self):
        self.assertEqual([r['id'] for r in propose([book(3), book(1), book(2)], {})], [3, 1, 2])


class ImportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.store = Store(Path(self.temp.name) / 'test.db')

    def test_goodreads_bom_quoted_isbn_and_read_shelf(self):
        rows, errors = parse_csv('\ufeffTitle,Author,ISBN13,Exclusive Shelf\nPiranesi,Susanna Clarke,="9781635575637",read\n'.encode(), 'goodreads')
        self.assertFalse(errors)
        self.assertEqual((rows[0]['isbn'], rows[0]['reading_status']), ('9781635575637', 'read'))

    def test_return_does_not_mean_read(self):
        rows, errors = parse_csv(b'Title,Author,Timestamp,Activity\nPiranesi,Susanna Clarke,2026-10-10,Returned\n', 'libby')
        self.assertFalse(errors)
        self.assertEqual(rows[0]['reading_status'], 'unreviewed')

    def test_bad_rows_reported(self):
        rows, errors = parse_csv(b'Title,Author,Timestamp,Activity\nA,B,2026-10-10,Mystery\nC,,bad-date,loan\n', 'libby')
        self.assertEqual((len(rows), len(errors)), (0, 2))

    def test_reimport_preserves_choices(self):
        with self.store.connect(write=True) as db:
            import_records(db, [record()])
            db.execute("UPDATE books SET reading_status='read',interest=5,pinned=1")
            added, _ = import_records(db, [record()])
            result = books(db)
        self.assertEqual((added, len(result), result[0]['reading_status'], result[0]['interest']), (0, 1, 'read', 5))

    def test_old_events_cannot_overwrite_latest_and_reimport_is_idempotent(self):
        rows, _ = parse_csv(b'Title,Author,Timestamp,Activity,Format\nPiranesi,Susanna Clarke,2026-10-10,Returned,ebook\nPiranesi,Susanna Clarke,2026-10-01,Borrowed,ebook\n', 'libby')
        with self.store.connect(write=True) as db:
            import_records(db, rows)
            added, events_added = import_records(db, rows)
            current = books(db)[0]['loans'][0]
            count = db.execute('SELECT count(*) FROM events').fetchone()[0]
        self.assertEqual((added, events_added, count, current['status']), (0, 0, 2, 'returned'))

    def test_physical_and_digital_loans_separate(self):
        with self.store.connect(write=True) as db:
            for source, fmt in [('nypl', 'print'), ('libby', 'ebook')]:
                event = {'status':'borrowed','activity':'Borrowed','at':'2026-10-10T00:00:00+00:00','format':fmt,'due_date':''}
                import_records(db, [record(source=source, event=event)])
            self.assertEqual(len(books(db)[0]['loans']), 2)

    def test_matches_isbn_across_title_variants(self):
        with self.store.connect(write=True) as db:
            a, b = record(), record(title='Piranesi: A Novel', source='libby')
            a['isbn'] = b['isbn'] = '9781635575637'
            import_records(db, [a, b])
            self.assertEqual(len(books(db)), 1)

    def test_rss_rejects_entities_and_nonfeeds(self):
        for xml in [b'<!DOCTYPE rss [<!ENTITY a "test">]><rss/>', b'<html/>']:
            with self.assertRaises(ValueError):
                parse_rss(xml)

    def test_rss_sync_only_adds_new_books(self):
        data = b'<rss><channel><item><title>Piranesi</title><author_name>Susanna Clarke</author_name><book_id>123</book_id></item></channel></rss>'
        with self.store.connect(write=True) as db:
            import_records(db, [record(title='An existing book')])
            set_meta(db, 'sync', {'user_id':'123','shelf':'to-read'})
        with patch('next_chapter.sync.fetch', return_value=parse_rss(data)):
            self.assertEqual(sync_once(self.store), 1)
            with self.store.connect() as db:
                before = revision(db)
            self.assertEqual(sync_once(self.store), 0)
            with self.store.connect() as db:
                self.assertEqual(revision(db), before)
        with self.store.connect() as db:
            self.assertEqual([b['title'] for b in books(db)], ['An existing book','Piranesi'])


class WebTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.app = create_app({'TESTING':True,'DEMO':True,'SECRET_KEY':'s'*40,
                               'DATABASE':str(Path(self.temp.name)/'test.db'),'START_POLLER':False})
        self.client = self.app.test_client()
        self.store = self.app.extensions['store']
        self.client.get('/')

    def form(self, **kwargs):
        with self.client.session_transaction() as sess:
            token = sess['csrf']
        with self.store.connect() as db:
            rev = revision(db)
        return dict(csrf=token,revision=str(rev),**kwargs)

    def add(self, title='Piranesi'):
        return self.client.post('/books/new',data=self.form(title=title,author='Susanna Clarke',interest='3'))

    def test_pages_render_and_backup_is_sqlite(self):
        for path in ['/', '/?view=all','/books/new','/imports','/settings','/history','/login','/export.json','/healthz','/examples/nypl.csv']:
            with self.subTest(path=path):
                with self.client.get(path) as response:
                    self.assertEqual(response.status_code, 200)
        self.assertTrue(self.client.get('/backup.sqlite3').data.startswith(b'SQLite format 3'))

    def test_add_preview_apply_undo(self):
        self.add('First book')
        self.add('Second book')
        result = self.client.post('/suggest', data=self.form(candidate_id='2'))
        self.assertEqual(result.status_code, 302)
        self.assertEqual(self.client.get(result.location).status_code, 200)
        self.assertEqual(self.client.post(result.location,data=self.form()).status_code,302)
        self.assertEqual(self.client.post('/undo',data=self.form()).status_code,302)
        self.assertEqual(self.client.post(result.location,data=self.form()).status_code,404)

    def test_stale_plan_cannot_clobber_new_book(self):
        first = self.add()
        self.add('Another book')
        self.assertEqual(self.client.post(first.location,data=self.form()).status_code,409)
        with self.store.connect() as db:
            self.assertEqual(len(books(db)),2)

    def test_stale_form_rejected(self):
        old = self.form(action='pin')
        self.add()
        self.assertEqual(self.client.post('/books/1/action',data=old).status_code,409)

    def test_csrf_and_html_escaping(self):
        self.assertEqual(self.client.post('/books/new',data={'title':'bad'}).status_code,400)
        self.add('<script>alert(1)</script>')
        response = self.client.get('/')
        self.assertNotIn(b'<script>alert(1)</script>',response.data)
        self.assertIn(b'&lt;script&gt;',response.data)
        self.assertIn("frame-ancestors 'none'",response.headers['Content-Security-Policy'])

    def test_import_preview_defers_changes(self):
        data = self.form(source='goodreads')
        data['file'] = (io.BytesIO(b'Title,Author,Exclusive Shelf\nPiranesi,Susanna Clarke,to-read\n'),'books.csv')
        response = self.client.post('/imports',data=data,content_type='multipart/form-data')
        self.assertEqual(response.status_code,302)
        with self.store.connect() as db:
            self.assertEqual(len(books(db)),0)
        self.assertEqual(self.client.get(response.location).status_code,200)
        self.client.post(response.location,data=self.form())
        with self.store.connect() as db:
            self.assertEqual(len(books(db)),1)

    def test_invalid_import_is_atomic(self):
        data = self.form(source='goodreads')
        data['file'] = (io.BytesIO(b'Title,Author\nGood,Author\nBad,\n'),'books.csv')
        self.assertEqual(self.client.post('/imports',data=data,content_type='multipart/form-data').status_code,400)
        with self.store.connect() as db:
            self.assertEqual(len(books(db)),0)

    def test_library_event_preserves_reading_status(self):
        self.add()
        self.assertEqual(self.client.post('/books/1/loan',data=self.form(source='nypl',status='returned',format='print')).status_code,302)
        with self.store.connect() as db:
            self.assertEqual(books(db)[0]['reading_status'],'to-read')
        self.assertEqual(self.client.get('/books/1').status_code,200)
        self.assertEqual(self.client.get('/history').status_code,200)

    def test_demo_and_preferences(self):
        self.assertEqual(self.client.post('/demo',data=self.form()).status_code,302)
        self.assertEqual(self.client.get('/').status_code,200)
        response = self.client.post('/settings',data=self.form(library_weight='0.5',length='short',mood='hopeful',shelf='to-read',user_id=''))
        self.assertEqual(response.status_code,302)
        self.assertEqual(self.client.get('/settings').status_code,200)

    def test_authentication_and_required_secrets(self):
        with self.assertRaises(RuntimeError):
            create_app({'DEMO':False,'SECRET_KEY':'','APP_PASSWORD':''})
        app = create_app({'TESTING':True,'DEMO':False,'SECRET_KEY':'s'*40,'APP_PASSWORD':'a-long-test-password',
                          'START_POLLER':False,'DATABASE':str(Path(self.temp.name)/'auth.db')})
        client = app.test_client()
        self.assertEqual(client.get('/export.json').status_code,302)
        self.assertEqual(client.get('/healthz').status_code,200)
        client.get('/login')
        with client.session_transaction() as sess:
            token = sess['csrf']
        self.assertEqual(client.post('/login',data={'csrf':token,'password':'a-long-test-password'}).status_code,302)
        self.assertEqual(client.get('/').status_code,200)
        with client.session_transaction() as sess:
            token = sess['csrf']
        client.post('/logout',data={'csrf':token})
        self.assertEqual(client.get('/').status_code,302)


if __name__ == '__main__':
    unittest.main()
