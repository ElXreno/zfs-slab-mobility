// Which slab caches pin otherwise-free pageblocks?
//
// /proc/kpageflags tells us a page belongs to slab but not which cache, and a
// block held by a handful of slab objects is exactly what stops the allocator
// from ever handing out an order-9 block. This walks every PFN, groups pages by
// pageblock, and reports which caches are squatting in nearly empty blocks.
// That is the list any slab-migration work has to target.
//
// The result is served from /proc/slabwho, one line per cache:
//
//     <name> <pages> <blocks> <hostage pages> <hostage blocks> <movable>
//
// A walk costs a fraction of a second, so results are cached and only redone
// when the caller asks for something older than refresh_ms.
//
// The layout of struct kmem_cache lives in mm/slab.h and is not exported, so the
// prefix up to ->name is mirrored here and validated against kmem_cache_size()
// before any of it is trusted. Anything reached through that mirror is read
// with copy_from_kernel_nofault(), because a cache can be destroyed while the
// walk is in flight and a stale pointer must not take the machine down.

#include <linux/codetag.h>
#include <linux/hash.h>
#include <linux/kernel.h>
#include <linux/memcontrol.h>
#include <linux/module.h>
#include <linux/mm.h>
#include <linux/mmzone.h>
#include <linux/mutex.h>
#include <linux/proc_fs.h>
#include <linux/reciprocal_div.h>
#include <linux/seq_file.h>
#include <linux/slab.h>
#include <linux/sort.h>
#include <linux/uaccess.h>
#include <linux/vmalloc.h>

static unsigned int hostage_max = 64;
module_param(hostage_max, uint, 0644);
MODULE_PARM_DESC(hostage_max, "a block with at most this many used pages counts as hostage");

/*
 * Attribution is served from its own file. It costs orders of magnitude more
 * than the cache walk, and a monitor polling twice a second must not pay for
 * something it did not ask for.
 */
static unsigned int sites_refresh_ms = 15000;
module_param(sites_refresh_ms, uint, 0644);
MODULE_PARM_DESC(sites_refresh_ms, "reuse the previous attribution if it is younger than this");

static unsigned int refresh_ms = 2000;
module_param(refresh_ms, uint, 0644);
MODULE_PARM_DESC(refresh_ms, "reuse the previous walk if it is younger than this");

struct kmem_cache_order_objects_mirror {
	unsigned int x;
};

/* Prefix of struct kmem_cache from mm/slab.h, up to and including ->name. */
struct kmem_cache_mirror {
	void __percpu *cpu_sheaves;
	slab_flags_t flags;
	unsigned long min_partial;
	unsigned int size;
	unsigned int object_size;
	struct reciprocal_value reciprocal_size;
	unsigned int offset;
	unsigned int sheaf_capacity;
	struct kmem_cache_order_objects_mirror oo;
	struct kmem_cache_order_objects_mirror min;
	gfp_t allocflags;
	int refcount;
	void (*ctor)(void *object);
	unsigned int inuse;
	unsigned int align;
	unsigned int red_left_pad;
	/*
	 * ->name follows, but the object mobility patch inserts five members
	 * before it, so where it sits depends on the kernel this is loaded
	 * into. Both layouts are tried against a probe cache at init and the
	 * offsets that match are kept in name_off and move_off.
	 */
};

/* Members the mobility patch adds between ->red_left_pad and ->name. */
struct kmem_cache_mobility_mirror {
	/*
	 * Typed as void * so this builds against a kernel without the patch
	 * that declares them; only whether ->move is set is ever asked.
	 */
	void *move;
	void *move_init;
	void *move_private;
	struct list_head mobile_list;
	unsigned int defrag_defer;
};

static size_t name_off;
static long move_off = -1;

/*
 * Mirror of struct slab from mm/slab.h. Only ->obj_exts and ->stride are wanted
 * and both alias fields of struct page, so the layout is checked against it
 * rather than trusted: obj_exts sits where memcg_data does, by static_assert in
 * the kernel's own header.
 */
struct slab_mirror {
	unsigned long flags;
	struct kmem_cache *slab_cache;
	struct list_head slab_list;
	void *freelist;
	union {
		unsigned long counters;
		struct {
			unsigned inuse:16;
			unsigned objects:15;
			unsigned frozen:1;
			unsigned int stride;
		};
	};
	unsigned int __page_type;
	atomic_t __page_refcount;
	unsigned long obj_exts;
};

#ifdef CONFIG_MEMCG
static_assert(offsetof(struct slab_mirror, obj_exts) ==
	      offsetof(struct page, memcg_data));
#endif
static_assert(offsetof(struct slab_mirror, slab_cache) ==
	      offsetof(struct page, compound_info));
static_assert(sizeof(struct slab_mirror) <= sizeof(struct page));

#define PAGES_PER_BLOCK (1UL << pageblock_order)
#define MAX_CACHES 512
#define MAX_SITES 1024
/* Open addressed, twice the entries, so a probe finds a free slot quickly. */
#define SITE_HASH_BITS 11
#define MAX_HOSTAGE (1U << 17)
#define NAME_LEN 40
#define SITE_LEN 64

struct cache_stat {
	const struct kmem_cache *cache;
	char name[NAME_LEN];
	u64 pages;
	u64 hostage_pages;
	u32 blocks;
	u32 hostage_blocks;
	u32 solo_blocks;
	bool mobile;
};

/*
 * Where an object was allocated from, for the objects sitting in hostage
 * blocks. SLUB merges caches of the same size into one kmem_cache and keeps
 * only the first name, so the cache table alone cannot say whether a block is
 * held by zswap or by ftrace. The codetag can, when the kernel carries one.
 */
struct site_stat {
	const struct kmem_cache *cache;
	const struct codetag *tag;
	char name[SITE_LEN];
	char file[SITE_LEN];
	unsigned int line;
	u64 objects;
	u32 blocks;
};

static DEFINE_MUTEX(walk_lock);
static struct site_stat *site_stats;
static unsigned int nr_sites;
static struct site_stat **site_hash;
static struct proc_dir_entry *sites_file;
static unsigned long sites_walked_at;
static bool sites_walked_once;
static unsigned long *hostage_pfns;
static unsigned int nr_hostage;
static bool sites_available;
static struct cache_stat *stats;
static unsigned int nr_stats;
static unsigned long walked_at;
static bool walked_once;
static u64 total_blocks, total_hostage_blocks, hostage_free_pages;

/*
 * How many caches hold each hostage block decides whether per-cache work can
 * ever pay off: a block pinned by three caches needs all three to become
 * evacuable before the allocator sees a single free pageblock.
 */
static u64 hostage_by_1, hostage_by_2, hostage_by_3plus;
static u64 hostage_nonslab;	/* has pages no slab cache owns: slab work cannot free it */
static u64 hostage_all_mobile;	/* every cache pinning it can already relocate */
static u64 walk_ns;

static struct cache_stat *stat_for(const struct kmem_cache *c)
{
	unsigned int i;

	for (i = 0; i < nr_stats; i++)
		if (stats[i].cache == c)
			return &stats[i];

	if (nr_stats == MAX_CACHES)
		return NULL;

	memset(&stats[nr_stats], 0, sizeof(stats[nr_stats]));
	stats[nr_stats].cache = c;
	return &stats[nr_stats++];
}

/* Returns the cache a slab page belongs to, or NULL if the page is not slab. */
static const struct kmem_cache *page_cache_of(struct page *page)
{
	struct page *head;
	unsigned long info;

	if (!PageSlab(page))
		return NULL;

	head = compound_head(page);
	info = head->compound_info;
	if (!info || (info & 1))
		return NULL;

	return (const struct kmem_cache *)info;
}

/*
 * Only copy_from_kernel_nofault() is exported to modules, so the string comes
 * across a byte at a time. It runs once per cache and the names are short.
 */
static bool copy_name_nofault(char *dst, const char *src, size_t len)
{
	size_t i;

	for (i = 0; i < len - 1; i++) {
		if (copy_from_kernel_nofault(&dst[i], &src[i], 1))
			return false;
		if (!dst[i])
			return true;
	}
	dst[len - 1] = '\0';
	return true;
}

/*
 * Fill in the parts of a cache we can only reach through the mirror. Both the
 * name pointer and the string behind it may already be freed by the time we
 * get here, so every step is fault tolerant and a failure just leaves the
 * cache unnamed.
 */
static void describe(struct cache_stat *st)
{
	const struct kmem_cache_mirror *m =
		(const struct kmem_cache_mirror *)st->cache;
	void *move;
	const char *name;

	if (st->name[0])
		return;

	if (copy_from_kernel_nofault(&name, (char *)m + name_off, sizeof(name)) ||
	    !copy_name_nofault(st->name, name, NAME_LEN) || !st->name[0])
		strscpy(st->name, "?", NAME_LEN);

	if (move_off < 0 ||
	    copy_from_kernel_nofault(&move, (char *)m + move_off, sizeof(move)))
		move = NULL;
	st->mobile = move != NULL;
}

static bool layout_is_sane(void)
{
	struct kmem_cache *probe = kmem_cache_create("slabwho_probe", 192, 0, SLAB_NO_MERGE, NULL);
	const struct kmem_cache_mirror *m;
	bool ok;

	if (!probe)
		return false;

	m = (const struct kmem_cache_mirror *)probe;
	ok = false;

	if (m->object_size == kmem_cache_size(probe)) {
		size_t plain = sizeof(struct kmem_cache_mirror);
		size_t mobile = plain + sizeof(struct kmem_cache_mobility_mirror);
		const char *name;

		/*
		 * Whichever offset yields the probe's own name is the layout
		 * this kernel was built with.
		 */
		name = *(const char * const *)((const char *)m + plain);
		if (name && !strcmp(name, "slabwho_probe")) {
			name_off = plain;
			move_off = -1;
			ok = true;
		} else {
			name = *(const char * const *)((const char *)m + mobile);
			if (name && !strcmp(name, "slabwho_probe")) {
				name_off = mobile;
				move_off = plain;
				ok = true;
			}
		}
	}

	if (ok)
		pr_info("slabwho: kmem_cache layout %s object mobility\n",
			move_off < 0 ? "without" : "with");
	else
		pr_warn("slabwho: mirrored struct kmem_cache does not match this kernel "
			"(object_size %u vs %u)\n",
			m->object_size, kmem_cache_size(probe));

	kmem_cache_destroy(probe);
	return ok;
}

#ifdef CONFIG_MEM_ALLOC_PROFILING

static int by_objects(const void *a, const void *b)
{
	const struct site_stat *x = a, *y = b;

	if (x->objects != y->objects)
		return y->objects > x->objects ? 1 : -1;
	return 0;
}

/*
 * Keyed by cache as well as by tag. SLUB hands several caches the same
 * kmem_cache, so the pair is what says which of them a site is filling, and
 * that is the only way to read a merged cache apart.
 */
static struct site_stat *site_for(const struct kmem_cache *cache,
				  const struct codetag *tag)
{
	u32 slot = hash_64((u64)(uintptr_t)tag ^ (u64)(uintptr_t)cache, SITE_HASH_BITS);
	unsigned int probes;

	/*
	 * Called once per object rather than once per page, so this cannot be
	 * the linear scan the cache table uses: on a fragmented machine that is
	 * a hundred million lookups over a thousand entries, under a mutex,
	 * while whoever is watching polls twice a second.
	 */
	for (probes = 0; probes < (1U << SITE_HASH_BITS); probes++) {
		struct site_stat *st = site_hash[slot];

		if (!st) {
			if (nr_sites == MAX_SITES)
				return NULL;
			st = &site_stats[nr_sites++];
			memset(st, 0, sizeof(*st));
			st->tag = tag;
			st->cache = cache;
			site_hash[slot] = st;
			return st;
		}
		if (st->tag == tag && st->cache == cache)
			return st;
		slot = (slot + 1) & ((1U << SITE_HASH_BITS) - 1);
	}
	return NULL;
}

static const char *cache_name_of(const struct kmem_cache *c)
{
	unsigned int i;

	for (i = 0; i < nr_stats; i++)
		if (stats[i].cache == c)
			return stats[i].name[0] ? stats[i].name : "?";
	return "?";
}

/*
 * A codetag lives in a module section that can go away with the module, so the
 * strings are copied the same fault tolerant way cache names are.
 */
static void describe_site(struct site_stat *st)
{
	struct codetag ct;
	const char *base;

	if (st->name[0])
		return;

	strscpy(st->name, "?", SITE_LEN);
	strscpy(st->file, "?", SITE_LEN);

	if (copy_from_kernel_nofault(&ct, st->tag, sizeof(ct)))
		return;

	st->line = ct.lineno;
	copy_name_nofault(st->name, ct.function, SITE_LEN);
	if (copy_name_nofault(st->file, ct.filename, SITE_LEN)) {
		base = strrchr(st->file, '/');
		if (base)
			memmove(st->file, base + 1, strlen(base));
	}
	if (!st->name[0])
		strscpy(st->name, "?", SITE_LEN);
}

/*
 * Objects whose first byte falls on this page. One straddling the boundary is
 * counted against the page it starts on, which is the same convention the page
 * level accounting above already uses.
 */
static void attribute_page(struct page *page, struct site_stat **seen,
			   unsigned int *nseen, unsigned int seen_max)
{
	struct page *head = compound_head(page);
	struct slab_mirror *slab = (struct slab_mirror *)head;
	const struct kmem_cache_mirror *cm;
	const struct kmem_cache *cache;
	unsigned long obj_exts, slab_addr, page_addr;
	unsigned int stride, size, objects, first, last, i, j;

	cache = page_cache_of(page);
	cm = (const struct kmem_cache_mirror *)cache;
	if (!cm)
		return;

	obj_exts = READ_ONCE(slab->obj_exts) & ~OBJEXTS_FLAGS_MASK;
	stride = slab->stride;
	size = cm->size;
	objects = slab->objects;

	/*
	 * Nothing here is reachable unless the kernel was built with allocation
	 * profiling and the vector was actually allocated for this slab. A
	 * nonsense stride means the mirrored layout does not match, and reading
	 * on would be indexing into whatever else lives there.
	 */
	if (!obj_exts || !objects || !size ||
	    stride < sizeof(struct slabobj_ext) || stride > 64)
		return;

	slab_addr = (unsigned long)page_address(head);
	page_addr = (unsigned long)page_address(page);
	if (!slab_addr || page_addr < slab_addr)
		return;

	first = DIV_ROUND_UP(page_addr - slab_addr, size);
	last = (page_addr + PAGE_SIZE - 1 - slab_addr) / size;
	if (last >= objects)
		last = objects - 1;

	for (i = first; i <= last; i++) {
		struct slabobj_ext *ext;
		struct site_stat *st;
		union codetag_ref ref;

		ext = (struct slabobj_ext *)(obj_exts + (unsigned long)stride * i);
		if (copy_from_kernel_nofault(&ref, &ext->ref, sizeof(ref)))
			continue;
		/* A freed object has its tag cleared, so this skips them. */
		if (!ref.ct)
			continue;

		st = site_for(cache, ref.ct);
		if (!st)
			continue;
		describe_site(st);
		st->objects++;

		for (j = 0; j < *nseen; j++)
			if (seen[j] == st)
				break;
		if (j == *nseen && *nseen < seen_max) {
			seen[(*nseen)++] = st;
			st->blocks++;
		}
	}
}

/*
 * Second pass, over the hostage blocks alone. Attributing every object in the
 * machine would multiply the walk by the number of objects per page, and the
 * only ones worth naming are those keeping a block from being handed out.
 */
static void walk_sites(void)
{
	struct site_stat *seen[64];
	unsigned int b;

	nr_sites = 0;
	if (!site_stats || !site_hash)
		return;
	memset(site_hash, 0, array_size(1U << SITE_HASH_BITS, sizeof(*site_hash)));

	for (b = 0; b < nr_hostage; b++) {
		unsigned long pfn, start = hostage_pfns[b];
		unsigned int nseen = 0;
		bool pfn_ok = false;

		for (pfn = start; pfn < start + PAGES_PER_BLOCK; pfn++) {
			struct page *page;

			if (!(pfn & (PAGES_PER_SUBSECTION - 1)))
				pfn_ok = pfn_valid(pfn);
			if (!pfn_ok)
				continue;
			page = pfn_to_page(pfn);
			if (!PageSlab(page))
				continue;
			attribute_page(page, seen, &nseen, ARRAY_SIZE(seen));
			/* Per page, not per block: a block can hold thousands
			 * of objects and each one is a fault tolerant read. */
			cond_resched();
		}
	}
	sort(site_stats, nr_sites, sizeof(*site_stats), by_objects, NULL);
	sites_available = nr_sites > 0;
	sites_walked_at = jiffies;
	sites_walked_once = true;
}

#else /* CONFIG_MEM_ALLOC_PROFILING */

static void walk_sites(void) { }

#endif

static int by_hostage(const void *a, const void *b)
{
	const struct cache_stat *x = a, *y = b;

	if (x->hostage_pages != y->hostage_pages)
		return y->hostage_pages > x->hostage_pages ? 1 : -1;
	return 0;
}

static void walk_memory(void)
{
	unsigned long pfn, block_start, scan_start, scan_end;
	struct cache_stat *seen[64];
	u32 seen_pages[64];
	unsigned int i;
	int nid;

	ktime_t t0 = ktime_get();

	nr_stats = 0;
	nr_hostage = 0;
	sites_available = false;
	total_blocks = total_hostage_blocks = hostage_free_pages = 0;
	hostage_by_1 = hostage_by_2 = hostage_by_3plus = 0;
	hostage_nonslab = hostage_all_mobile = 0;

	/*
	 * max_pfn is not exported, so the range comes from the zones. Not from
	 * node_spanned_pages: with memory hotplug that covers everything the
	 * kernel could ever address, which here is 2^34 pages of mostly nothing
	 * and turns a tenth of a second of work into eight seconds.
	 */
	for_each_online_node(nid) {
		pg_data_t *pgdat = NODE_DATA(nid);
		unsigned long done = 0;
		enum zone_type z;

		for (z = 0; z < MAX_NR_ZONES; z++) {
		struct zone *zone = &pgdat->node_zones[z];

		if (!populated_zone(zone))
			continue;

		scan_start = zone->zone_start_pfn;
		scan_end = zone_end_pfn(zone);

		block_start = ALIGN_DOWN(scan_start, PAGES_PER_BLOCK);
		if (block_start < done)
			block_start = done;

		for (; block_start < scan_end; block_start += PAGES_PER_BLOCK) {
			unsigned int used = 0, freep = 0, nseen = 0, nonslab = 0;
			bool valid_block = false, pfn_ok = false;

			memset(seen_pages, 0, sizeof(seen_pages));

			for (pfn = block_start; pfn < block_start + PAGES_PER_BLOCK; pfn++) {
				const struct kmem_cache *c;
				struct page *page;

				/*
				 * pfn_valid() takes and drops a preemption-disabled
				 * section on every call, which costs more than the rest
				 * of this loop put together when it runs once per page.
				 * Validity only changes at subsection boundaries, so ask
				 * once per subsection and reuse the answer.
				 */
				if (!(pfn & (PAGES_PER_SUBSECTION - 1)))
					pfn_ok = pfn_valid(pfn);
				if (!pfn_ok)
					continue;
				page = pfn_to_page(pfn);
				valid_block = true;

				/* Slab pages carry no refcount here, so ask about slab
				 * membership before deciding a page looks free. */
				c = page_cache_of(page);
				if (!c) {
					/*
					 * PG_buddy sits only on the first page of a free
					 * run, so the rest have to be recognised by their
					 * missing refcount. Compound pages have to be
					 * excluded from that: their tails carry no reference
					 * either, and ZFS scatter ABD is millions of them,
					 * which made this report twice as many hostage
					 * blocks as really existed.
					 */
					if (PageBuddy(page) ||
					    (!page_count(page) && !PageCompound(page))) {
						freep++;
						continue;
					}
					used++;
					/*
					 * Anything on the LRU is migrated by compaction
					 * without help, so it is not what keeps the block
					 * from being freed. Only the rest counts as an
					 * obstacle slab work can never remove.
					 */
					if (!PageLRU(page))
						nonslab++;
					continue;
				}
				used++;

				for (i = 0; i < nseen; i++)
					if (seen[i]->cache == c)
						break;
				if (i == nseen) {
					if (nseen == ARRAY_SIZE(seen))
						continue;
					seen[nseen] = stat_for(c);
					if (!seen[nseen])
						continue;
					seen_pages[nseen] = 0;
					nseen++;
				}
				seen_pages[i]++;
			}

			if (!valid_block)
				continue;
			total_blocks++;

			for (i = 0; i < nseen; i++) {
				describe(seen[i]);
				seen[i]->pages += seen_pages[i];
				seen[i]->blocks++;
			}

			if (used && used <= hostage_max) {
				bool all_mobile = true;

				total_hostage_blocks++;
				hostage_free_pages += freep;
				if (hostage_pfns && nr_hostage < MAX_HOSTAGE)
					hostage_pfns[nr_hostage++] = block_start;
				for (i = 0; i < nseen; i++) {
					seen[i]->hostage_pages += seen_pages[i];
					seen[i]->hostage_blocks++;
					if (!seen[i]->mobile)
						all_mobile = false;
				}

				if (nonslab)
					hostage_nonslab++;
				else if (all_mobile)
					hostage_all_mobile++;

				/*
				 * A block one cache has to itself is the only kind a
				 * single new callback can hand back to the allocator.
				 */
				if (nseen == 1 && !nonslab)
					seen[0]->solo_blocks++;

				if (nonslab || nseen > 2)
					hostage_by_3plus++;
				else if (nseen == 2)
					hostage_by_2++;
				else
					hostage_by_1++;
			}

			cond_resched();
		}

		done = block_start;
		}
	}

	sort(stats, nr_stats, sizeof(*stats), by_hostage, NULL);
	walk_ns = ktime_to_ns(ktime_sub(ktime_get(), t0));
	walked_at = jiffies;
	walked_once = true;
}

static void ensure_walk(void)
{
	if (!walked_once ||
	    time_after(jiffies, walked_at + msecs_to_jiffies(refresh_ms)))
		walk_memory();
}

static void *slabwho_start(struct seq_file *s, loff_t *pos)
{
	mutex_lock(&walk_lock);
	ensure_walk();

	if (*pos == 0)
		return SEQ_START_TOKEN;
	if (*pos - 1 >= nr_stats)
		return NULL;
	return &stats[*pos - 1];
}

static void *slabwho_next(struct seq_file *s, void *v, loff_t *pos)
{
	(*pos)++;
	if (*pos - 1 >= nr_stats)
		return NULL;
	return &stats[*pos - 1];
}

/*
 * Attribution, in its own file and on its own clock. Walking it costs a fault
 * tolerant read per object rather than per page, so it must never ride along
 * with the cache table that a monitor refreshes twice a second.
 */
static void *sites_start(struct seq_file *s, loff_t *pos)
{
	mutex_lock(&walk_lock);
	ensure_walk();
	if (!sites_walked_once ||
	    time_after(jiffies, sites_walked_at + msecs_to_jiffies(sites_refresh_ms)))
		walk_sites();

	if (*pos == 0)
		return SEQ_START_TOKEN;
	if (*pos - 1 >= nr_sites)
		return NULL;
	return &site_stats[*pos - 1];
}

static void *sites_next(struct seq_file *s, void *v, loff_t *pos)
{
	(*pos)++;
	if (*pos - 1 >= nr_sites)
		return NULL;
	return &site_stats[*pos - 1];
}

static int sites_show(struct seq_file *s, void *v)
{
	const struct site_stat *si = v;

	if (v == SEQ_START_TOKEN) {
		seq_printf(s, "# sites %u age_ms %u\n", sites_available ? nr_sites : 0,
			   jiffies_to_msecs(jiffies - sites_walked_at));
		return 0;
	}
	seq_printf(s, "@ %llu %u %s %s %s:%u\n", si->objects, si->blocks,
		   cache_name_of(si->cache), si->name, si->file, si->line);
	return 0;
}

static void slabwho_stop(struct seq_file *s, void *v)
{
	mutex_unlock(&walk_lock);
}

static int slabwho_show(struct seq_file *s, void *v)
{
	const struct cache_stat *st = v;

	if (v == SEQ_START_TOKEN) {
		seq_printf(s, "# blocks %llu hostage_blocks %llu hostage_free_pages %llu hostage_max %u age_ms %u walk_us %llu\n",
			   total_blocks, total_hostage_blocks, hostage_free_pages,
			   hostage_max, jiffies_to_msecs(jiffies - walked_at),
			   walk_ns / 1000);
		seq_printf(s, "# pinned_by_one %llu pinned_by_two %llu pinned_by_more %llu nonslab %llu all_mobile %llu\n",
			   hostage_by_1, hostage_by_2, hostage_by_3plus,
			   hostage_nonslab, hostage_all_mobile);
		return 0;
	}

	seq_printf(s, "%s %llu %u %llu %u %d %u\n", st->name[0] ? st->name : "?",
		   st->pages, st->blocks, st->hostage_pages, st->hostage_blocks,
		   st->mobile ? 1 : 0, st->solo_blocks);
	return 0;
}

static const struct seq_operations slabwho_ops = {
	.start = slabwho_start,
	.next = slabwho_next,
	.stop = slabwho_stop,
	.show = slabwho_show,
};

static const struct seq_operations sites_ops = {
	.start = sites_start,
	.next = sites_next,
	.stop = slabwho_stop,
	.show = sites_show,
};

static int __init slabwho_init(void)
{
	if (!layout_is_sane())
		return -EINVAL;

	stats = vzalloc(array_size(MAX_CACHES, sizeof(*stats)));
	if (!stats)
		return -ENOMEM;

	if (!proc_create_seq("slabwho", 0444, NULL, &slabwho_ops)) {
		vfree(stats);
		return -ENOMEM;
	}

#ifdef CONFIG_MEM_ALLOC_PROFILING
	/* Optional: without them the cache table still works, unattributed. */
	hostage_pfns = vzalloc(array_size(MAX_HOSTAGE, sizeof(*hostage_pfns)));
	site_stats = vzalloc(array_size(MAX_SITES, sizeof(*site_stats)));
	site_hash = vzalloc(array_size(1U << SITE_HASH_BITS, sizeof(*site_hash)));
	if (hostage_pfns && site_stats && site_hash)
		sites_file = proc_create_seq("slabwho_sites", 0444, NULL, &sites_ops);
#endif
	return 0;
}

static void __exit slabwho_exit(void)
{
	if (sites_file)
		remove_proc_entry("slabwho_sites", NULL);
	remove_proc_entry("slabwho", NULL);
	vfree(site_hash);
	vfree(site_stats);
	vfree(hostage_pfns);
	vfree(stats);
}

module_init(slabwho_init);
module_exit(slabwho_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("which slab caches pin nearly-empty pageblocks");
