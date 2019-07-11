/*
Copyright 2016 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package goolib

import (
	"archive/tar"
	"bytes"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/blang/semver"
)

func mkVer(sem string, rel int64) Version {
	return Version{
		Semver: semver.MustParse(sem),
		GsVer:  rel,
	}
}

func TestParseVersion(t *testing.T) {
	table := []struct {
		ver string
		res Version
	}{
		{"1.2.3@4", mkVer("1.2.3", 4)},
		{"1.2.3", mkVer("1.2.3", 0)},
		{"1.02.3", mkVer("1.2.3", 0)},
		{"1.2@7", mkVer("0.1.2", 7)},
		{"1.2.0", mkVer("1.2.0", 0)},
		{"1.2.3+1", mkVer("1.2.3+1", 0)},
		{"1.2.03-1", mkVer("1.2.3-1", 0)},
		{"1.2.3+4@5", mkVer("1.2.3+4", 5)},
	}
	for _, tt := range table {
		v, err := ParseVersion(tt.ver)
		if err != nil {
			t.Errorf("ParseVersion(%v): %v", tt.ver, err)
		}
		if !reflect.DeepEqual(v, tt.res) {
			t.Errorf("ParseVersion(%v) = %v, want %v", tt.ver, v, tt.res)
		}
	}
}

func TestBadParseVersion(t *testing.T) {
	table := []struct {
		ver string
	}{
		{"1.2.d3@4"},
		{"1.2.3@4d"},
		{"1.2.3.4@4"},
	}
	for _, tt := range table {
		if _, err := ParseVersion(tt.ver); err == nil {
			t.Error("expected but did not receive version error")
		}
	}
}

func TestVerify(t *testing.T) {
	gs := GooSpec{
		PackageSpec: &PkgSpec{
			Arch:            "noarch",
			Name:            "name",
			Version:         "1.2.3@4",
			PkgDependencies: map[string]string{"name": "1.2.3@4"},
		},
	}
	if err := gs.verify(); err != nil {
		t.Error(err)
	}
}

func TestBadVerify(t *testing.T) {
	table := []struct {
		gs   GooSpec
		werr string
	}{
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "noarch",
				Version: "1.2.3@4",
			},
		}, "no name defined in package spec"},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch: "noarch",
				Name: "name",
			},
		}, "version string empty"},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "noarch",
				Name:    "name",
				Version: "1.2.3:4d",
			},
		}, `Invalid character(s) found in patch number "3:4d"`},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "something",
				Name:    "name",
				Version: "1.2.3@4",
			},
		}, `invalid architecture: "something"`},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:            "noarch",
				Name:            "name",
				Version:         "1.2.3@4",
				PkgDependencies: map[string]string{"name": "1.2.3h@4"},
			},
		}, `can't parse version "1.2.3h@4" for dependancy "name": Invalid character(s) found in patch number "3h"`},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "noarch",
				Name:    "name",
				Version: "1.2.3@4",
				Tags: map[string][]byte{
					"a": nil,
					"b": nil,
					"c": nil,
					"d": nil,
					"e": nil,
					"f": nil,
					"g": nil,
					"h": nil,
					"i": nil,
					"j": nil,
					"k": nil,
				},
			},
		}, "too many tags"},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "noarch",
				Name:    "name",
				Version: "1.2.3@4",
				Tags: map[string][]byte{
					"it little profits that an idle king, by this still hearth, among these barren craigs, I mete and dole unequal laws unto a savage race who hoards, and feeds, and sleeps, and knows not me": []byte("right?"),
				},
			},
		}, "tag key too large"},
		{GooSpec{
			PackageSpec: &PkgSpec{
				Arch:    "noarch",
				Name:    "name",
				Version: "1.2.3@4",
				Tags: map[string][]byte{
					"text": lotsOBytes,
				},
			},
		}, `tag "text" too large`},
	}
	for _, tt := range table {
		err := tt.gs.verify()
		if err == nil {
			t.Errorf("expected %q, got no error", tt.werr)
			continue
		}
		if !strings.Contains(err.Error(), tt.werr) {
			t.Errorf("did not get expected error: got %q, want %q", err.Error(), tt.werr)
		}
	}
}

func TestCompare(t *testing.T) {
	table := []struct {
		v1     string
		v2     string
		result int
	}{
		{"1.2.3@1", "1.2.3@2", -1},
		{"1.2.4@1", "1.2.3@2", 1},
		{"1.2.3", "1.2.3", 0},
	}
	for _, tt := range table {
		c, err := Compare(tt.v1, tt.v2)
		if err != nil {
			t.Error(err)
		}
		if c != tt.result {
			t.Errorf("package comparison unexpected result: got %v, want %v", c, tt.result)
		}
	}
}

func TestBadCompare(t *testing.T) {
	if _, err := Compare("1.2a.3", "1.2.3"); err == nil {
		t.Error("expected error, bad semver version")
	}
}

func TestWritePackageSpec(t *testing.T) {
	es := &PkgSpec{
		Name:    "test",
		Version: "1.2.3@4",
		Arch:    "noarch",
	}

	buf := new(bytes.Buffer)
	w := tar.NewWriter(buf)

	if err := WritePackageSpec(w, es); err != nil {
		t.Errorf("error writing GooSpec: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("error closing zip writer: %v", err)
	}

	spec, err := ReadPackageSpec(buf)
	if err != nil {
		t.Errorf("ReadPackageSpec: %v", err)
	}

	if !reflect.DeepEqual(spec, es) {
		t.Errorf("did not get expected spec: got %v, want %v", spec, es)
	}
}

func TestSortVersions(t *testing.T) {
	got := SortVersions([]string{"1.2.3@4", "1.5.0", "1.0.0", "1.0", "1.2.A", "1.2.3@1"})
	want := []string{"1.0", "1.0.0", "1.2.3@1", "1.2.3@4", "1.5.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("did not get expected list: got %v, want %v", got, want)
	}
}

func TestUnmarshalGooSpec(t *testing.T) {
	c1 := []byte(`{
  "name": "pkg",
  "version": "1.2.3@4",
  "arch": "noarch",
  "releaseNotes": [
    "1.2.3@4 - something new",
    "1.2.3@4 - something"
  ],
  "description": "blah blah",
  "owners": "someone",
  "install": {
    "path": "install.ps1"
  },
  "sources":[ {
    "include":["**"],
    "root":"some/place"
  } ]
}`)

	want := &GooSpec{
		Sources: []PkgSources{
			{
				Include: []string{"**"},
				Root:    "some/place",
			}},
		PackageSpec: &PkgSpec{
			Name:         "pkg",
			Version:      "1.2.3@4",
			Arch:         "noarch",
			ReleaseNotes: []string{"1.2.3@4 - something new", "1.2.3@4 - something"},
			Description:  "blah blah",
			Owners:       "someone",
			Install: ExecFile{
				Path: "install.ps1",
			},
		},
	}

	got, err := unmarshalGooSpec(c1, nil)
	if err != nil {
		t.Fatalf("error running unmarshalGooSpec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("did not get expected GooSpec, got: \n%+v\nwant: \n%+v", got, want)
	}
}

func TestNoPathTraversal(t *testing.T) {
	c1 := []byte(`{
  "name": "pkg",
  "version": "1.2.3@4",
  "arch": "noarch",
  "releaseNotes": [],
  "description": "blah blah",
  "owners": "someone",
  "install": {
    "path": "../../../../install.ps1"
  },
  "sources": []
}`)
	var fail []byte
	if runtime.GOOS == "windows" {
		fail = []byte(`{
		"name": "pkg",
		"version": "1.2.3@4",
		"arch": "noarch",
		"releaseNotes": [],
		"description": "blah blah",
		"owners": "someone",
		"install": {
			"path": "Z:\usr\bin\sudo"
		},
		"sources": []
	}`)
	} else {
		fail = []byte(`{
		"name": "pkg",
		"version": "1.2.3@4",
		"arch": "noarch",
		"releaseNotes": [],
		"description": "blah blah",
		"owners": "someone",
		"install": {
			"path": "/usr/bin/sudo"
		},
		"sources": []
	}`)
	}
	got, err := UnmarshalPackageSpec(c1)
	if err != nil {
		t.Fatalf("error running unmarshalGooSpec: %v", err)
	}
	path := got.Install.Path
	if strings.Contains(path, "..") {
		t.Errorf("install path %s allows path traversal", path)
	}
	if filepath.IsAbs(path) {
		t.Errorf("install path %s is absolute", path)
	}

	_, err = UnmarshalPackageSpec(fail)
	if err == nil {
		t.Errorf("goospec containing absolute path successfully unmarshalled %s", fail)
	}

}

func TestMarshal(t *testing.T) {
	rs := &RepoSpec{
		Checksum: "asdkgaksd545as4d6",
		Source:   "blah",
		PackageSpec: &PkgSpec{
			Name:         "pkg",
			Version:      "1.2.3@4",
			Arch:         "noarch",
			ReleaseNotes: []string{"1.2.3@4 - something new", "1.2.3@4 - something"},
			Description:  "blah blah",
			Owners:       "someone",
			Replaces:     []string{"foo"},
			Conflicts:    []string{"bar"},
			Install: ExecFile{
				Path: "install.ps1",
			},
		},
	}
	want := []byte(`{
  "Checksum": "asdkgaksd545as4d6",
  "Source": "blah",
  "PackageSpec": {
    "Name": "pkg",
    "Version": "1.2.3@4",
    "Arch": "noarch",
    "ReleaseNotes": [
      "1.2.3@4 - something new",
      "1.2.3@4 - something"
    ],
    "Description": "blah blah",
    "Owners": "someone",
    "Replaces": [
      "foo"
    ],
    "Conflicts": [
      "bar"
    ],
    "Install": {
      "Path": "install.ps1"
    },
    "Uninstall": {},
    "Verify": {}
  }
}`)
	got, err := rs.Marshal()
	if err != nil {
		t.Fatalf("error running Marshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("did not get expected JSON, got: \n%s\nwant: \n%s", got, want)
	}
}

var lotsOBytes = []byte(`I wish either my father or my mother, or indeed both of them, as they were in duty both equally bound to it, had minded what they were about when they begot me; had they duly consider'd how much depended upon what they were then doing;—that not only the production of a rational Being was concerned in it, but that possibly the happy formation and temperature of his body, perhaps his genius and the very cast of his mind;—and, for aught they knew to the contrary, even the fortunes of his whole house might take their turn from the humours and dispositions which were then uppermost;—Had they duly weighed and considered all this, and proceeded accordingly,—I am verily persuaded I should have made a quite different figure in the world, from that in which the reader is likely to see me.—Believe me, good folks, this is not so inconsiderable a thing as many of you may think it;—you have all, I dare say, heard of the animal spirits, as how they are transfused from father to son, &c. &c.—and a great deal to that purpose:—Well, you may take my word, that nine parts in ten of a man's sense or his nonsense, his successes and miscarriages in this world depend upon their motions and activity, and the different tracks and trains you put them into, so that when they are once set a-going, whether right or wrong, 'tis not a half-penny matter,—away they go cluttering like hey-go mad; and by treading the same steps over and over again, they presently make a road of it, as plain and as smooth as a garden-walk, which, when they are once used to, the Devil himself sometimes shall not be able to drive them off it.

Pray my Dear, quoth my mother, have you not forgot to wind up the clock?—Good G..! cried my father, making an exclamation, but taking care to moderate his voice at the same time,—Did ever woman, since the creation of the world, interrupt a man with such a silly question? Pray, what was your father saying?—Nothing.

—Then, positively, there is nothing in the question that I can see, either good or bad.—Then, let me tell you, Sir, it was a very unseasonable question at least,—because it scattered and dispersed the animal spirits, whose business it was to have escorted and gone hand in hand with the Homunculus, and conducted him safe to the place destined for his reception.

The Homunculus, Sir, in however low and ludicrous a light he may appear, in this age of levity, to the eye of folly or prejudice;—to the eye of reason in scientific research, he stands confess'd—a Being guarded and circumscribed with rights.—The minutest philosophers, who by the bye, have the most enlarged understandings, (their souls being inversely as their enquiries) shew us incontestably, that the Homunculus is created by the same hand,—engender'd in the same course of nature,—endow'd with the same loco-motive powers and faculties with us:—That he consists as we do, of skin, hair, fat, flesh, veins, arteries, ligaments, nerves, cartilages, bones, marrow, brains, glands, genitals, humours, and articulations;—is a Being of as much activity,—and in all senses of the word, as much and as truly our fellow-creature as my Lord Chancellor of England.—He may be benefitted,—he may be injured,—he may obtain redress; in a word, he has all the claims and rights of humanity, which Tully, Puffendorf, or the best ethick writers allow to arise out of that state and relation.

Now, dear Sir, what if any accident had befallen him in his way alone!—or that through terror of it, natural to so young a traveller, my little Gentleman had got to his journey's end miserably spent;—his muscular strength and virility worn down to a thread;—his own animal spirits ruffled beyond description,—and that in this sad disorder'd state of nerves, he had lain down a prey to sudden starts, or a series of melancholy dreams and fancies, for nine long, long months together.—I tremble to think what a foundation had been laid for a thousand weaknesses both of body and mind, which no skill of the physician or the philosopher could ever afterwards have set thoroughly to rights.

To my uncle Mr. Toby Shandy do I stand indebted for the preceding anecdote, to whom my father, who was an excellent natural philosopher, and much given to close reasoning upon the smallest matters, had oft, and heavily complained of the injury; but once more particularly, as my uncle Toby well remember'd, upon his observing a most unaccountable obliquity, (as he call'd it) in my manner of setting up my top, and justifying the principles upon which I had done it,—the old gentleman shook his head, and in a tone more expressive by half of sorrow than reproach,—he said his heart all along foreboded, and he saw it verified in this, and from a thousand other observations he had made upon me, That I should neither think nor act like any other man's child:—But alas! continued he, shaking his head a second time, and wiping away a tear which was trickling down his cheeks, My Tristram's misfortunes began nine months before ever he came into the world.

—My mother, who was sitting by, look'd up, but she knew no more than her backside what my father meant,—but my uncle, Mr. Toby Shandy, who had been often informed of the affair,—understood him very well.

I know there are readers in the world, as well as many other good people in it, who are no readers at all,—who find themselves ill at ease, unless they are let into the whole secret from first to last, of every thing which concerns you.

It is in pure compliance with this humour of theirs, and from a backwardness in my nature to disappoint any one soul living, that I have been so very particular already. As my life and opinions are likely to make some noise in the world, and, if I conjecture right, will take in all ranks, professions, and denominations of men whatever,—be no less read than the Pilgrim's Progress itself—and in the end, prove the very thing which Montaigne dreaded his Essays should turn out, that is, a book for a parlour-window;—I find it necessary to consult every one a little in his turn; and therefore must beg pardon for going on a little farther in the same way: For which cause, right glad I am, that I have begun the history of myself in the way I have done; and that I am able to go on, tracing every thing in it, as Horace says, ab Ovo.

Horace, I know, does not recommend this fashion altogether: But that gentleman is speaking only of an epic poem or a tragedy;—(I forget which,) besides, if it was not so, I should beg Mr. Horace's pardon;—for in writing what I have set about, I shall confine myself neither to his rules, nor to any man's rules that ever lived.

To such however as do not choose to go so far back into these things, I can give no better advice than that they skip over the remaining part of this chapter; for I declare before-hand, 'tis wrote only for the curious and inquisitive.

—Shut the door.—

I was begot in the night betwixt the first Sunday and the first Monday in the month of March, in the year of our Lord one thousand seven hundred and eighteen. I am positive I was.—But how I came to be so very particular in my account of a thing which happened before I was born, is owing to another small anecdote known only in our own family, but now made publick for the better clearing up this point.

My father, you must know, who was originally a Turkey merchant, but had left off business for some years, in order to retire to, and die upon, his paternal estate in the county of ——, was, I believe, one of the most regular men in every thing he did, whether 'twas matter of business, or matter of amusement, that ever lived. As a small specimen of this extreme exactness of his, to which he was in truth a slave, he had made it a rule for many years of his life,—on the first Sunday-night of every month throughout the whole year,—as certain as ever the Sunday-night came,—to wind up a large house-clock, which we had standing on the back-stairs head, with his own hands:—And being somewhere between fifty and sixty years of age at the time I have been speaking of,—he had likewise gradually brought some other little family concernments to the same period, in order, as he would often say to my uncle Toby, to get them all out of the way at one time, and be no more plagued and pestered with them the rest of the month.

It was attended but with one misfortune, which, in a great measure, fell upon myself, and the effects of which I fear I shall carry with me to my grave; namely, that from an unhappy association of ideas, which have no connection in nature, it so fell out at length, that my poor mother could never hear the said clock wound up,—but the thoughts of some other things unavoidably popped into her head—& vice versa:—Which strange combination of ideas, the sagacious Locke, who certainly understood the nature of these things better than most men, affirms to have produced more wry actions than all other sources of prejudice whatsoever.

But this by the bye.

Now it appears by a memorandum in my father's pocket-book, which now lies upon the table, 'That on Lady-day, which was on the 25th of the same month in which I date my geniture,—my father set upon his journey to London, with my eldest brother Bobby, to fix him at Westminster school;' and, as it appears from the same authority, 'That he did not get down to his wife and family till the second week in May following,'—it brings the thing almost to a certainty. However, what follows in the beginning of the next chapter, puts it beyond all possibility of a doubt.

—But pray, Sir, What was your father doing all December, January, and February?—Why, Madam,—he was all that time afflicted with a Sciatica.

On the fifth day of November, 1718, which to the aera fixed on, was as near nine kalendar months as any husband could in reason have expected,—was I Tristram Shandy, Gentleman, brought forth into this scurvy and disastrous world of ours.—I wish I had been born in the Moon, or in any of the planets, (except Jupiter or Saturn, because I never could bear cold weather) for it could not well have fared worse with me in any of them (though I will not answer for Venus) than it has in this vile, dirty planet of ours,—which, o' my conscience, with reverence be it spoken, I take to be made up of the shreds and clippings of the rest;—not but the planet is well enough, provided a man could be born in it to a great title or to a great estate; or could any how contrive to be called up to public charges, and employments of dignity or power;—but that is not my case;—and therefore every man will speak of the fair as his own market has gone in it;—for which cause I affirm it over again to be one of the vilest worlds that ever was made;—for I can truly say, that from the first hour I drew my breath in it, to this, that I can now scarce draw it at all, for an asthma I got in scating against the wind in Flanders;—I have been the continual sport of what the world calls Fortune; and though I will not wrong her by saying, She has ever made me feel the weight of any great or signal evil;—yet with all the good temper in the world I affirm it of her, that in every stage of my life, and at every turn and corner where she could get fairly at me, the ungracious duchess has pelted me with a set of as pitiful misadventures and cross accidents as ever small Hero sustained.

In the beginning of the last chapter, I informed you exactly when I was born; but I did not inform you how. No, that particular was reserved entirely for a chapter by itself;—besides, Sir, as you and I are in a manner perfect strangers to each other, it would not have been proper to have let you into too many circumstances relating to myself all at once.

—You must have a little patience. I have undertaken, you see, to write not only my life, but my opinions also; hoping and expecting that your knowledge of my character, and of what kind of a mortal I am, by the one, would give you a better relish for the other: As you proceed farther with me, the slight acquaintance, which is now beginning betwixt us, will grow into familiarity; and that unless one of us is in fault, will terminate in friendship.—O diem praeclarum!—then nothing which has touched me will be thought trifling in its nature, or tedious in its telling. Therefore, my dear friend and companion, if you should think me somewhat sparing of my narrative on my first setting out—bear with me,—and let me go on, and tell my story my own way:—Or, if I should seem now and then to trifle upon the road,—or should sometimes put on a fool's cap with a bell to it, for a moment or two as we pass along,—don't fly off,—but rather courteously give me credit for a little more wisdom than appears upon my outside;—and as we jog on, either laugh with me, or at me, or in short do any thing,—only keep your temper.

In the same village where my father and my mother dwelt, dwelt also a thin, upright, motherly, notable, good old body of a midwife, who with the help of a little plain good sense, and some years full employment in her business, in which she had all along trusted little to her own efforts, and a great deal to those of dame Nature,—had acquired, in her way, no small degree of reputation in the world:—by which word world, need I in this place inform your worship, that I would be understood to mean no more of it, than a small circle described upon the circle of the great world, of four English miles diameter, or thereabouts, of which the cottage where the good old woman lived is supposed to be the centre?—She had been left it seems a widow in great distress, with three or four small children, in her forty-seventh year; and as she was at that time a person of decent carriage,—grave deportment,—a woman moreover of few words and withal an object of compassion, whose distress, and silence under it, called out the louder for a friendly lift: the wife of the parson of the parish was touched with pity; and having often lamented an inconvenience to which her husband's flock had for many years been exposed, inasmuch as there was no such thing as a midwife, of any kind or degree, to be got at, let the case have been never so urgent, within less than six or seven long miles riding; which said seven long miles in dark nights and dismal roads, the country thereabouts being nothing but a deep clay, was almost equal to fourteen; and that in effect was sometimes next to having no midwife at all; it came into her head, that it would be doing as seasonable a kindness to the whole parish, as to the poor creature herself, to get her a little instructed in some of the plain principles of the business, in order to set her up in it. As no woman thereabouts was better qualified to execute the plan she had formed than herself, the gentlewoman very charitably undertook it; and having great influence over the female part of the parish, she found no difficulty in effecting it to the utmost of her wishes. In truth, the parson join'd his interest with his wife's in the whole affair, and in order to do things as they should be, and give the poor soul as good a title by law to practise, as his wife had given by institution,—he cheerfully paid the fees for the ordinary's licence himself, amounting in the whole, to the sum of eighteen shillings and four pence; so that betwixt them both, the good woman was fully invested in the real and corporal possession of her office, together with all its rights, members, and appurtenances whatsoever.

These last words, you must know, were not according to the old form in which such licences, faculties, and powers usually ran, which in like cases had heretofore been granted to the sisterhood. But it was according to a neat Formula of Didius his own devising, who having a particular turn for taking to pieces, and new framing over again all kind of instruments in that way, not only hit upon this dainty amendment, but coaxed many of the old licensed matrons in the neighbourhood, to open their faculties afresh, in order to have this wham-wham of his inserted.

I own I never could envy Didius in these kinds of fancies of his:—But every man to his own taste.—Did not Dr. Kunastrokius, that great man, at his leisure hours, take the greatest delight imaginable in combing of asses tails, and plucking the dead hairs out with his teeth, though he had tweezers always in his pocket? Nay, if you come to that, Sir, have not the wisest of men in all ages, not excepting Solomon himself,—have they not had their Hobby-Horses;—their running horses,—their coins and their cockle-shells, their drums and their trumpets, their fiddles, their pallets,—their maggots and their butterflies?—and so long as a man rides his Hobby-Horse peaceably and quietly along the King's highway, and neither compels you or me to get up behind him,—pray, Sir, what have either you or I to do with it?`)
