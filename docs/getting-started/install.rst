Install Ethermint
=================

Via Package Manager
--------------------

Note: these commands will also install ``tendermint`` as a required binary.

Linux
~~~~~

::

    apt-get install ethermint

MacOS
~~~~~

::

    brew install ethermint

Windows
~~~~~~~

::

    choco install ethermint


Download Binary
---------------

See the `tendermint website <https://tendermint.com/downloads>`__ to download the binaries for each platform.


From Source
-----------

On all platforms, you'll need ``golang`` `installed <https://golang.org/doc/install>`__. Then you can do:

::

    go get -u -d github.com/ya-enot/etherus
    go get -u -d github.com/ya-enot/tendermint
    cd $GOPATH/src/github.com/ya-enot/etherus
    make install
    cd ../tendermint
    make install

Hang tight, the build may take awhile. A tool called ``glide`` will also be installed to manage the dependencies.
